package main

import (
	"context"
	"fmt"
	"log"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/aggregates"
	"github.com/jamestryand/pocketcqrs/authforward"
	"github.com/jamestryand/pocketcqrs/authverify"
	"github.com/jamestryand/pocketcqrs/batching"
	"github.com/jamestryand/pocketcqrs/commandqueue"
	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
	"github.com/jamestryand/pocketcqrs/gateway"
	"github.com/jamestryand/pocketcqrs/idempotency"
	"github.com/jamestryand/pocketcqrs/migrations"
	"github.com/jamestryand/pocketcqrs/outbound"
	"github.com/jamestryand/pocketcqrs/projections"
	"github.com/jamestryand/pocketcqrs/reactors"
	"github.com/jamestryand/pocketcqrs/roles"
	"github.com/jamestryand/pocketcqrs/writeguard"
)

// idempotencyRetention bounds how long a gateway idempotency record is kept
// before StartPruner deletes it. A client retrying after an ambiguous
// timeout does so within seconds to minutes, not days; 24h leaves ample
// margin without letting the table grow unboundedly.
const idempotencyRetention = 24 * time.Hour

// Node roles for --cqrsRole (see docs/reference/cli.md's "Multi-node"
// section for the single-writer/multi-reader design). roleMaster is this
// project's entire behavior before this flag existed; roleSecondary is
// new and additive.
const (
	roleMaster    = "master"
	roleSecondary = "secondary"
)

// components is filled during bootstrap, before the server starts.
type components struct {
	app         core.App
	store       *events.Store
	idempotency *idempotency.Store
	registry    *decider.Registry
	engine      *consumers.Engine
	httpFns     *functions.HTTPRegistry
	jsProjs     []*functions.JSProjection
	fnRuntime   *functions.GojaRuntime

	// batchWriter backs command batching (item 4, on by default), or is
	// nil when --cqrsCommandBatching=false or this node is a roleSecondary,
	// which never runs its own writer.
	batchWriter *batching.Writer

	// role mirrors --cqrsRole (roleMaster or roleSecondary), set once
	// immediately after ParseFlags, same lifecycle as tutorial below.
	role string

	// verifier backs --cqrsVerifyAuth (F-13): remote token verification
	// against the master, with verifyCache holding the bounded-TTL verdicts.
	// Both nil except on a secondary running verify mode.
	verifier    *authverify.Verifier
	verifyCache *authverify.Cache

	// outbound backs the $http binding, or is nil when
	// --cqrsAllowOutboundHTTP is absent (the default). Held here because a
	// hot reload builds a fresh runtime and must carry it across — otherwise
	// $http would work until the first reload and then vanish.
	outbound *outbound.Client

	// jsDeciders tracks JS-managed aggregates (vs built-in Go deciders)
	// with their active specs, for hot-reload swaps and upcaster rebuilds.
	jsDeciders map[string]*functions.DeciderSpec
	// jsReactors tracks the active JS reactors so a reload can unregister
	// exactly what it registered (their checkpoint keys are stable, so a
	// swapped reactor resumes where the old code left off).
	jsReactors []*functions.ReactorSpec
	// cronJobs lists the registered cron job ids ("fn:"+name).
	cronJobs []string
	// reloadMu serializes hot reloads.
	reloadMu sync.Mutex

	// tutorial mirrors --tutorial. Set once immediately after ParseFlags,
	// before anything reads it, so OnBootstrap and every subcommand's RunE
	// see the same answer. When false this repo's example domains are not
	// wired at all — the platform ships empty.
	tutorial bool

	// schemaDefaultRule mirrors --cqrsSchemaDefaultRule (Item 9). Set once
	// immediately after ParseFlags, same lifecycle as tutorial above. Read
	// by ReconcileSchemas at boot and on every maintenance reload, so a hot
	// reload sees the same deployment-wide default it booted with.
	schemaDefaultRule string
}

func main() {
	// `skill` copies files out of the binary and touches nothing else, so run
	// it before PocketBase exists.
	//
	// Going through app.Start() would BOOTSTRAP THE WHOLE APPLICATION first —
	// creating pb_data/ and applying migrations in whatever directory the user
	// happens to be standing in — as a side effect of asking for a file to be
	// copied. PocketBase only skips bootstrap for --help and --version
	// (pocketbase.go, skipBootstrap) and offers no per-command opt-out, so the
	// short-circuit has to live here. The command is still registered on
	// RootCmd below so that `pocketcqrs --help` lists it.
	if len(os.Args) > 1 && os.Args[1] == "skill" {
		sc := newSkillCommand()
		sc.SetArgs(os.Args[2:])
		if err := sc.Execute(); err != nil {
			os.Exit(1)
		}
		return
	}

	// `schema import` is a pure file-to-file transformation (see
	// newSchemaImportCommand's doc comment) with no dependency on a running
	// platform, so it gets the same short-circuit as `skill` above, for the
	// same reason: going through app.Start() bootstraps a full pb_data/
	// before any RunE runs, with no per-command opt-out, and a refused
	// import left one behind despite writing nothing itself (F-10).
	// Short-circuiting also fixes F-9 for this command: PocketBase's own
	// pb.Execute() discards RootCmd.Execute()'s return value, so a RunE
	// error used to print correctly but never reach a non-zero exit code.
	// Running Execute() directly here lets main() act on it.
	if len(os.Args) > 2 && os.Args[1] == "schema" && os.Args[2] == "import" {
		ic := newSchemaImportCommand()
		ic.SetArgs(os.Args[3:])
		if err := ic.Execute(); err != nil {
			os.Exit(1)
		}
		return
	}

	app := pocketbase.New()
	c := &components{}

	var gatewayCfg gateway.Config
	app.RootCmd.PersistentFlags().BoolVar(
		&gatewayCfg.AllowAnonymous,
		"cqrsAllowAnonymous",
		false,
		"allow anonymous CQRS command execution (dev only; no actor metadata is stamped)",
	)
	app.RootCmd.PersistentFlags().StringVar(
		&gatewayCfg.ExternalCallerCollection,
		"cqrsExternalCallerCollection",
		"",
		"name of a PocketBase auth collection whose authenticated records are external service integrations (e.g. pocketcqrs-extensions' extcaller), not end users or reactors; unset (default) leaves actor/causation/correlation handling exactly as before this existed",
	)

	// Outbound HTTP for the effect/reactor tiers. Off by default: with these
	// unset, core's posture is exactly what it was before the feature existed.
	var allowOutboundHTTP bool
	app.RootCmd.PersistentFlags().BoolVar(
		&allowOutboundHTTP,
		"cqrsAllowOutboundHTTP",
		false,
		"allow event, cron and reactor functions to call out over HTTP via $http; not //@trigger http functions, and never deciders or projections",
	)
	var outboundHosts []string
	app.RootCmd.PersistentFlags().StringArrayVar(
		&outboundHosts,
		"cqrsOutboundHost",
		nil,
		"a hostname $http may call, repeatable; the list is deployment-wide, not per-function (no entries = nothing permitted)",
	)
	var allowPrivateOutbound bool
	app.RootCmd.PersistentFlags().BoolVar(
		&allowPrivateOutbound,
		"cqrsAllowPrivateOutbound",
		false,
		"let $http reach loopback and private ranges (dev and internal services; link-local stays blocked)",
	)

	var strictBoot bool
	app.RootCmd.PersistentFlags().BoolVar(
		&strictBoot,
		"cqrsStrictBoot",
		false,
		"abort startup if a JS decider fails validation (default: skip the decider and keep serving)",
	)

	var functionsDir string
	app.RootCmd.PersistentFlags().StringVar(
		&functionsDir,
		"functionsDir",
		"pb_functions",
		"the directory with the user defined JS functions",
	)
	var tutorial bool
	app.RootCmd.PersistentFlags().BoolVar(
		&tutorial,
		"tutorial",
		false,
		"register this repo's example domains (task, order) and their collections; off by default — pocketcqrs ships empty",
	)

	// Item 9: //@schema-created collections default to public ListRule/
	// ViewRule (writes stay write-guarded regardless). This flag changes
	// the deployment-wide default; a //@rule directive in a function file
	// overrides it per collection. Neither ever touches an already-existing
	// collection's rule — reconcile stays additive-only, same guarantee
	// field changes already get.
	var schemaDefaultRule string
	app.RootCmd.PersistentFlags().StringVar(
		&schemaDefaultRule,
		"cqrsSchemaDefaultRule",
		"",
		`default ListRule/ViewRule for newly created //@schema collections: "public" (default), `+
			`"authenticated", or a raw PocketBase rule expression. A //@rule <collection> <value> `+
			"directive overrides this per collection. Never changes an existing collection's rule.",
	)

	// Node role for the single-writer/multi-reader deployment
	// (docs/reference/cli.md's "Multi-node" section): a secondary polls a
	// replicated events.db read-only instead of appending to it, and
	// checkpoints its own consumers separately since the read-only store
	// can't hold them (see consumers.NewEngineWithCheckpoints, and
	// events.ErrReadOnly below).
	// What this flag ALONE does NOT do: forward commands or auth traffic
	// to a master (item 3 -- see --cqrsMasterAddr below, a separate,
	// already-built flag) — a secondary with no --cqrsMasterAddr set
	// refuses commands outright (503) rather than silently accepting
	// ones it can't durably apply.
	var role string
	app.RootCmd.PersistentFlags().StringVar(
		&role,
		"cqrsRole",
		roleMaster,
		"this node's role: "+roleMaster+" (default, appends to events.db) or "+roleSecondary+
			" (polls a replicated events.db read-only; commands are refused, not forwarded)",
	)
	var vfs string
	app.RootCmd.PersistentFlags().StringVar(
		&vfs,
		"cqrsVFS",
		"",
		"the SQLite VFS name events.db is opened through when --cqrsRole="+roleSecondary+
			" (e.g. a Litestream-provided VFS already registered with the driver); empty opens the plain file read-only",
	)
	var eventsPathOverride string
	app.RootCmd.PersistentFlags().StringVar(
		&eventsPathOverride,
		"cqrsEventsPath",
		"",
		"path to events.db, overriding the default of <dir>/events.db; a "+roleSecondary+
			" needs this to point at the master's replicated file rather than its own local one",
	)
	// Command forwarding (item 3): only meaningful on a secondary. Empty
	// (the default) leaves a secondary refusing commands outright (see
	// events.ErrReadOnly in gateway.go) -- a deliberate choice, e.g. a
	// pure reporting replica that should never accept write traffic even
	// via forwarding.
	var masterAddr string
	app.RootCmd.PersistentFlags().StringVar(
		&masterAddr,
		"cqrsMasterAddr",
		"",
		"the master's base URL (e.g. http://master:8090); when set on --cqrsRole="+roleSecondary+
			", commands are proxied there instead of refused. No effect on "+roleMaster,
	)

	// Auth-collection forwarding (item 5's remainder, F-12): deliberately a
	// SEPARATE opt-in from --cqrsMasterAddr, not bundled with it, found
	// necessary while wiring this up -- once PocketBase's own login flow
	// forwards to the master, every token a client gets is signed with the
	// MASTER's JWT secret, which a secondary's own LOCAL routes cannot
	// verify (data.db, where the signing secret lives, is never synced
	// between nodes). That silently breaks every authenticated LOCAL read
	// on a secondary, which command-forwarding alone does not. An operator
	// enabling --cqrsMasterAddr for CQRS commands should not unknowingly
	// also break that just by doing so.
	var forwardAuth bool
	app.RootCmd.PersistentFlags().BoolVar(
		&forwardAuth,
		"cqrsForwardAuth",
		false,
		"route PocketBase's own native auth-collection traffic (_users/_superusers/etc, not just "+
			"CQRS commands) to the master too, since a secondary's copy of those collections is a "+
			"different table, not a stale replica. Requires --cqrsMasterAddr. WARNING: without "+
			"--cqrsVerifyAuth, enabling this breaks authenticated LOCAL reads on this secondary -- "+
			"every token becomes master-signed and unverifiable here (F-13); add --cqrsVerifyAuth "+
			"so this node verifies those tokens against the master instead.",
	)

	// Remote token verification (F-13's fix): a secondary cannot verify any
	// token itself -- the key material (record TokenKey + collection secret)
	// lives only in each node's own, never-replicated data.db. Shape C':
	// ask the master, cache the verdict for a bounded TTL. Implies
	// --cqrsForwardAuth, because only master-minted tokens can pass remote
	// verification -- a coherent multi-node auth mode, not two flags to
	// mis-combine.
	var verifyAuth bool
	app.RootCmd.PersistentFlags().BoolVar(
		&verifyAuth,
		"cqrsVerifyAuth",
		false,
		"verify bearer tokens against the master (with a bounded local verdict cache) so this "+
			"secondary's own authenticated LOCAL reads work. Requires --cqrsMasterAddr; implies "+
			"--cqrsForwardAuth (only master-minted tokens can verify remotely). The ops routes "+
			"re-verify uncached on every request so an admin revocation bites immediately.",
	)
	var verifyCacheTTL time.Duration
	app.RootCmd.PersistentFlags().DurationVar(
		&verifyCacheTTL,
		"cqrsVerifyCacheTTL",
		5*time.Minute,
		"how long a --cqrsVerifyAuth verdict is trusted before re-checking with the master, always "+
			"additionally capped by the token's own exp claim. Also the revocation-lag bound: a "+
			"token the master has revoked can keep reading here for up to this long.",
	)
	var verifyGrace time.Duration
	app.RootCmd.PersistentFlags().DurationVar(
		&verifyGrace,
		"cqrsVerifyGrace",
		0,
		"how far past a verdict's expiry it may still be served when the master is UNREACHABLE "+
			"(never past the token's own exp). 0 (the default) fails closed: expired verdict + "+
			"unreachable master = 503. Opting in trades a bounded revocation lag for reads that "+
			"keep working through a master outage.",
	)

	// Command batching (item 4): ON by default -- F-5's fix (queue-depth
	// admission control, --cqrsCommandQueueMaxDepth) is only built on this
	// path, so making it the default collapses two admission-control
	// mechanisms (a bounded semaphore for the direct path, plus this
	// queue's own depth check) down to one. Batching preserves the exact
	// synchronous (events, error) response contract the direct path always
	// returned, so this default is not a client-visible contract change --
	// only variable added latency under load (bounded by
	// --cqrsBatchTimeout) and, if --cqrsCommandQueueMaxDepth is set, a 503
	// once that many commands are unfinished. No effect on roleSecondary,
	// which never runs its own writer (it forwards or refuses; see
	// --cqrsMasterAddr).
	var commandBatching bool
	app.RootCmd.PersistentFlags().BoolVar(
		&commandBatching,
		"cqrsCommandBatching",
		true,
		"accumulate decided commands into batches committed in one transaction, instead of one "+
			"transaction per command; raises write throughput under load at the cost of variable "+
			"added latency, bounded by --cqrsBatchTimeout. On by default; pass "+
			"--cqrsCommandBatching=false for the old one-transaction-per-command path (that path "+
			"has no admission control of its own -- see F-5).",
	)
	var batchTimeout time.Duration
	app.RootCmd.PersistentFlags().DurationVar(
		&batchTimeout,
		"cqrsBatchTimeout",
		10*time.Second,
		"how long a command waits for its batch to commit when --cqrsCommandBatching is set "+
			"before returning 504; matches events.db's own busy_timeout by default",
	)
	var commandQueueMaxDepth int
	app.RootCmd.PersistentFlags().IntVar(
		&commandQueueMaxDepth,
		"cqrsCommandQueueMaxDepth",
		0,
		"shed new commands with 503 (Retry-After: 1) once this many are enqueued and not yet "+
			"committed (F-5). Only takes effect when batching is active (on by default). "+
			"0 disables shedding.",
	)

	app.RootCmd.AddCommand(newProjectionCommand(c))
	app.RootCmd.AddCommand(newDeadletterCommand(c))
	app.RootCmd.AddCommand(newDryrunCommand(c))
	app.RootCmd.AddCommand(newSystemCommand(c))
	app.RootCmd.AddCommand(newCatalogCommand(c))
	app.RootCmd.AddCommand(newPackCommand(c, &functionsDir))
	app.RootCmd.AddCommand(newSchemaCommand(c))
	app.RootCmd.AddCommand(newSkillCommand())
	app.RootCmd.ParseFlags(os.Args[1:])
	c.tutorial = tutorial
	c.schemaDefaultRule = schemaDefaultRule
	if role != roleMaster && role != roleSecondary {
		log.Fatalf("invalid --cqrsRole %q (want %q or %q)", role, roleMaster, roleSecondary)
	}
	c.role = role

	if role != roleSecondary && app.RootCmd.PersistentFlags().Changed("cqrsVFS") {
		log.Printf("warning: --cqrsVFS is set but --cqrsRole=%s ignores it (only %s opens events.db through a VFS)", role, roleSecondary)
	}
	if role == roleSecondary {
		for _, name := range []string{"cqrsCommandBatching", "cqrsBatchTimeout", "cqrsCommandQueueMaxDepth"} {
			if app.RootCmd.PersistentFlags().Changed(name) {
				log.Printf("warning: --%s is set but --cqrsRole=%s never runs its own writer (a secondary forwards or refuses commands, it does not batch or queue them)", name, roleSecondary)
			}
		}
	}

	if forwardAuth && masterAddr == "" {
		log.Fatal("invalid flags: --cqrsForwardAuth requires --cqrsMasterAddr")
	}
	if verifyAuth && masterAddr == "" {
		log.Fatal("invalid flags: --cqrsVerifyAuth requires --cqrsMasterAddr")
	}
	if verifyAuth && role != roleSecondary {
		log.Printf("warning: --cqrsVerifyAuth is set but --cqrsRole=%s ignores it (only %s verifies remotely)", role, roleSecondary)
		verifyAuth = false
	}
	if verifyAuth && !forwardAuth {
		// implied, not required: remote verification only works for
		// master-minted tokens, so login has to forward too -- and with
		// verification in place, forwarding no longer breaks local reads,
		// which was the only reason the flags were ever separate
		forwardAuth = true
		log.Print("--cqrsVerifyAuth implies --cqrsForwardAuth: auth flows forward to the master so every token is master-minted and remotely verifiable")
	}
	if !verifyAuth {
		for _, name := range []string{"cqrsVerifyCacheTTL", "cqrsVerifyGrace"} {
			if app.RootCmd.PersistentFlags().Changed(name) {
				log.Printf("warning: --%s has no effect without --cqrsVerifyAuth", name)
			}
		}
	}

	var masterURL *url.URL
	if masterAddr != "" {
		if role != roleSecondary {
			log.Printf("warning: --cqrsMasterAddr is set but --cqrsRole=%s ignores it (only %s forwards)", role, roleSecondary)
		} else {
			parsed, err := url.Parse(masterAddr)
			if err != nil || parsed.Scheme == "" || parsed.Host == "" {
				log.Fatalf("invalid --cqrsMasterAddr %q: want a full base URL, e.g. http://master:8090", masterAddr)
			}
			masterURL = parsed
			gatewayCfg.Forward = httputil.NewSingleHostReverseProxy(masterURL)
		}
	}

	// Registering the example migrations is a decision, taken here: an
	// unregistered migration is never applied AND never recorded, so the
	// examples can be switched off and on again without either direction
	// being a one-way door. See migrations.RegisterExamples.
	if c.tutorial {
		migrations.RegisterExamples()
	}

	// Item 11: the roles collection (capability-based ops/dashboard access,
	// see ops.go's capOps* constants) is always registered, unlike the
	// --tutorial examples above -- it's this project's own feature, not
	// switchable content, so every deployment gets it.
	roles.RegisterCollection()

	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		// run the default bootstrap first: it creates the data dir and,
		// crucially, opens the PocketBase DBs — ReconcileSchemas below
		// needs a live DB
		if err := e.Next(); err != nil {
			return err
		}

		dataDir := e.App.DataDir()
		if err := os.MkdirAll(dataDir, os.ModePerm); err != nil {
			return err
		}
		c.app = e.App

		// collections-as-DDL: apply the shipped app migrations now. Serve
		// would run them later (apis.Serve), which is too late for
		// ReconcileSchemas — relation targets may be migration-created
		// collections. Idempotent; system migrations already ran in e.Next().
		//
		// Runs identically on every role: it only ever touches data.db (via
		// PocketBase's own migration runner), never events.db. Each node's
		// data.db is local and always writable regardless of role — that is
		// not a new risk this flag introduces, it's what a single instance
		// already does today (docs/reference/cli.md's "Multi-node" section).
		if err := e.App.RunAppMigrations(); err != nil {
			return err
		}

		// event store (source of truth), next to PocketBase's data.db by
		// default. roleSecondary polls a replica read-only instead of
		// appending to it — see events.OpenReadOnly's doc comment for what
		// that means for every write method on the returned Store — and
		// needs --cqrsEventsPath to point at the master's file, since a
		// secondary's own data.db must stay local and independent.
		eventsPath := eventsPathOverride
		if eventsPath == "" {
			eventsPath = filepath.Join(dataDir, "events.db")
		}
		var store *events.Store
		var err error
		if c.role == roleSecondary {
			var opts []events.OpenOption
			if vfs != "" {
				opts = append(opts, events.WithVFS(vfs))
			}
			store, err = events.OpenReadOnly(eventsPath, opts...)
		} else {
			store, err = events.Open(eventsPath)
		}
		if err != nil {
			return err
		}
		c.store = store

		// idempotency records for the command gateway: a separate small
		// SQLite file, deliberately off events.db's hot append path.
		idem, err := idempotency.Open(filepath.Join(dataDir, "idempotency.db"))
		if err != nil {
			return err
		}
		c.idempotency = idem
		gatewayCfg.Idempotency = idem

		// remote token verification (F-13): the verdict cache is another
		// small dedicated SQLite file -- and SQLite rather than a map so a
		// verdict cached before a master outage survives this node
		// restarting during it (the exact scenario --cqrsVerifyGrace exists
		// for).
		if verifyAuth && masterURL != nil {
			cache, err := authverify.OpenCache(filepath.Join(dataDir, "authverify.db"))
			if err != nil {
				return err
			}
			c.verifyCache = cache
			c.verifier = authverify.New(masterURL, cache, verifyCacheTTL, verifyGrace)
		}

		// write side: deciders + command handling. The platform registers no
		// aggregates of its own — task and order are example content, and
		// without --tutorial their names are free for a JS decider to claim.
		c.registry = decider.NewRegistry(store)
		if c.tutorial {
			aggregates.RegisterAll(c.registry)
		}
		c.jsDeciders = map[string]*functions.DeciderSpec{}

		// the gateway rejects domain commands while the system is in
		// maintenance mode (hot reload of schema-bearing functions)
		gatewayCfg.Mode = store.Mode

		logger := e.App.Logger()

		// durable consumption of the log (projections + functions). A
		// secondary's store can't hold its own checkpoints (it's read-only —
		// events.SaveCheckpoint would fail every tick with events.ErrReadOnly),
		// so it checkpoints to a separate, ordinary, locally writable store
		// instead — the shape consumers.NewEngineWithCheckpoints exists for.
		engineLogger := func(msg string, args ...any) { logger.Warn(msg, args...) }
		if c.role == roleSecondary {
			checkpoints, err := events.Open(filepath.Join(dataDir, "checkpoints.db"))
			if err != nil {
				return err
			}
			c.engine = consumers.NewEngineWithCheckpoints(store, checkpoints, engineLogger)
		} else {
			c.engine = consumers.NewEngine(store, engineLogger)
		}

		// command batching (item 4): on by default, and never on a
		// secondary -- it has no writable store to enqueue into or commit
		// against (a secondary forwards or refuses; see --cqrsMasterAddr).
		if commandBatching && c.role != roleSecondary {
			queue, err := commandqueue.Open(filepath.Join(dataDir, "commandqueue.db"))
			if err != nil {
				return err
			}
			c.batchWriter = batching.NewWriter(store, queue, c.registry, engineLogger)
			c.batchWriter.MaxDepth = commandQueueMaxDepth
			gatewayCfg.Batching = c.batchWriter
			gatewayCfg.BatchTimeout = batchTimeout
		}

		// read side: projections into PocketBase collections
		projs := c.allProjections(e.App)
		for _, p := range projs {
			c.engine.Register(p)
		}

		// functions (FaaS): JS functions loaded from functionsDir
		rt := functions.NewGojaRuntime(
			func(msg string, args ...any) { logger.Info(msg, args...) })
		rt.SetReader(functions.NewAppReader(e.App))
		rt.SetStore(store)

		// outbound HTTP, only if asked for. An empty allow-list with the
		// flag on permits NOTHING and says so — "no entries" is not "no
		// restriction", which is the reading that made an empty writeguard
		// list guard everything in v0.4.0.
		if allowOutboundHTTP {
			client, err := outbound.New(outbound.Config{
				AllowedHosts: outboundHosts,
				AllowPrivate: allowPrivateOutbound,
				Timeout:      functions.OutboundTimeout,
				MaxInFlight:  functions.OutboundMaxInFlight,
				MaxBodyBytes: functions.OutboundMaxBodyBytes,
			})
			if err != nil {
				return fmt.Errorf("outbound HTTP config: %w", err)
			}
			c.outbound = client
			rt.SetOutbound(client)
			if len(outboundHosts) == 0 {
				logger.Warn("outbound HTTP is enabled but no --cqrsOutboundHost was given, " +
					"so every $http call will be refused")
			} else {
				logger.Info("outbound HTTP enabled for effect and reactor functions",
					"hosts", strings.Join(outboundHosts, ","),
					"allowPrivate", allowPrivateOutbound)
			}
		}

		c.fnRuntime = rt
		loaded, err := functions.LoadDir(rt, e.App, functionsDir)
		if err != nil {
			return err
		}
		c.httpFns = loaded.HTTP

		// JS deciders (tier 3): dry-run validated against existing history
		// at boot, then registered alongside the Go deciders. Failures are
		// refused loudly — the rest of the system keeps serving, unless
		// --cqrsStrictBoot is set (boot aborts instead).
		var validatedDeciders []*functions.DeciderSpec
		for _, spec := range loaded.Deciders {
			if c.registry.Has(spec.Aggregate) {
				if strictBoot {
					return fmt.Errorf("strict boot: JS decider aggregate %q collides with an existing decider", spec.Aggregate)
				}
				logger.Error("JS decider aggregate collides with an existing decider, skipped",
					"aggregate", spec.Aggregate)
				continue
			}
			if err := functions.ValidateDeciderSpec(store, spec); err != nil {
				if strictBoot {
					return fmt.Errorf("strict boot: JS decider %q failed validation: %w", spec.Aggregate, err)
				}
				logger.Error("JS decider failed validation, NOT registered",
					"aggregate", spec.Aggregate, "error", err)
				continue
			}
			c.registry.RegisterUntyped(spec.Aggregate, spec.UntypedDecider())
			c.jsDeciders[spec.Aggregate] = spec
			validatedDeciders = append(validatedDeciders, spec)
			logger.Info("JS decider active", "aggregate", spec.Aggregate)
		}

		// store-level upcasting: the validated deciders' transform chains
		// compose into the store's read path, so every consumer (deciders,
		// projections, functions, reactors) sees events at their latest
		// schema version. Only validated specs contribute.
		c.store.SetUpcaster(functions.BuildUpcaster(validatedDeciders))

		// JS projection schemas are materialized at boot (a restart IS the
		// maintenance window), additively: create/extend, never drop
		if err := functions.ReconcileSchemas(e.App, loaded.Projections, c.schemaDefaultRule); err != nil {
			return err
		}

		// engine registration order matters: Go projections first, then JS
		// projections (which may read Go-maintained collections), then
		// reactors and effect functions
		for _, spec := range loaded.Projections {
			c.jsProjs = append(c.jsProjs, spec.Consumer())
		}
		for _, p := range c.jsProjs {
			c.engine.Register(p)
		}

		// sagas: reactors dispatch follow-up commands through the registry.
		// The fulfillment saga is example content — it wires the example
		// order aggregate to the example task one, so it only exists when
		// they do.
		if c.tutorial {
			c.engine.Register(reactors.AsConsumer(reactors.Fulfillment(), c.registry,
				func(msg string, args ...any) { logger.Info(msg, args...) },
				func(msg string, args ...any) { logger.Warn(msg, args...) }))
		}

		// JS reactors (tier 4): same dispatch rule as the Go ones, reached
		// from a function file. The registry must be installed BEFORE they
		// are registered as consumers — a reactor without one fails loudly
		// rather than quietly doing nothing.
		rt.SetRegistry(c.registry)
		rt.SetWarn(func(msg string, args ...any) { logger.Warn(msg, args...) })
		// //@dispatches gate (F-2): the registry is complete here — deciders
		// were registered above — so the live registry IS the right thing to
		// validate against at boot. A reload cannot say the same; see
		// prospectiveCommands in reload.go.
		var activeReactors []*functions.ReactorSpec
		for _, spec := range loaded.Reactors {
			if err := functions.ValidateReactorSpec(c.registry, spec); err != nil {
				if strictBoot {
					return fmt.Errorf("strict boot: JS reactor %q failed validation: %w", spec.Reactor, err)
				}
				logger.Error("JS reactor failed validation, NOT registered",
					"reactor", spec.Reactor, "error", err)
				continue
			}
			activeReactors = append(activeReactors, spec)
		}
		c.jsReactors = activeReactors
		for _, spec := range activeReactors {
			c.engine.Register(spec)
			logger.Info("JS reactor active", "reactor", spec.Reactor, "on", spec.EventTypes)
		}

		// write-guard: no out-of-band writes on projection-owned collections
		guarded := c.allProjections(e.App)
		for _, p := range c.jsProjs {
			guarded = append(guarded, p)
		}
		cols := projections.GuardedCollections(guarded...)

		// An example collection can outlive its projection: boot once with
		// --tutorial and then without, and `tasks` is still there with
		// nothing maintaining it. Keep guarding it — an unmaintained read
		// model is bad, an unmaintained read model anyone can write to is
		// worse — but say so, because a collection that quietly stopped
		// updating is the kind of thing found months later.
		if !c.tutorial {
			var orphaned []string
			for _, name := range exampleCollections(e.App) {
				if slices.Contains(cols, name) {
					continue // something else owns this name now, and maintains it
				}
				if _, err := e.App.FindCollectionByNameOrId(name); err != nil {
					continue // never created, or since dropped
				}
				orphaned = append(orphaned, name)
			}
			if len(orphaned) > 0 {
				cols = append(cols, orphaned...)
				logger.Warn("example collections exist but nothing is projecting into them: "+
					"they stay write-guarded and read-only, and will not be updated. "+
					"Start with --tutorial to maintain them again, or delete them.",
					"collections", strings.Join(orphaned, ", "))
			}
		}

		// _authOrigins is PocketBase's own collection, not projection-owned,
		// but safe to guard alongside them — see writeguard.AuthOrigins (F-6).
		writeguard.Register(e.App, append(cols, writeguard.AuthOrigins)...)

		// effect functions: durable delivery through the consumers engine
		for _, fc := range rt.Consumers() {
			c.engine.Register(fc)
		}

		// cron functions: scheduled by PocketBase's cron service
		for _, job := range rt.CronJobs() {
			id := "fn:" + job.Name
			if err := e.App.Cron().Add(id, job.Schedule, job.Fire); err != nil {
				return err
			}
			c.cronJobs = append(c.cronJobs, id)
		}

		return nil
	})

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		gateway.RegisterRoutes(e, c.registry, gatewayCfg)
		// the verify oracle answers on every node (a secondary delegates to
		// the master), so it is registered unconditionally; the verifying
		// middleware only exists where there is a verifier
		authverify.RegisterEndpoint(e, c.verifier)
		if c.verifier != nil {
			authverify.Register(e, c.verifier)
		}
		if forwardAuth && gatewayCfg.Forward != nil {
			// item 5's remainder (F-12): PocketBase's own native auth
			// traffic (_users/_superusers/etc.) needs the same
			// forward-to-master treatment as CQRS commands, for a
			// different reason -- a secondary's copy of those collections
			// isn't a stale replica, it's a different table, so serving
			// auth locally would read the wrong data before any write
			// even happens. Reuses the exact same reverse proxy gatewayCfg
			// .Forward already is. Deliberately gated on its OWN flag, not
			// just gatewayCfg.Forward != nil -- see --cqrsForwardAuth's
			// own help text for why it can't be bundled with
			// --cqrsMasterAddr automatically.
			authforward.Register(e, gatewayCfg.Forward)
		}
		functions.RegisterHTTPRoutes(e, c.httpFns, !gatewayCfg.AllowAnonymous)
		registerReloadRoute(e, c, functionsDir)
		registerFunctionAdminRoutes(e, c, functionsDir)
		registerCatalogRoute(e, c)
		registerOpsRoutes(e, c)
		c.engine.Start(context.Background())
		if c.batchWriter != nil {
			c.batchWriter.Start(context.Background())
		}
		c.idempotency.StartPruner(context.Background(), time.Hour, idempotencyRetention,
			func(msg string, args ...any) { e.App.Logger().Warn(msg, args...) })
		if c.verifyCache != nil {
			c.verifyCache.StartPruner(context.Background(), time.Hour, verifyGrace,
				func(msg string, args ...any) { e.App.Logger().Warn(msg, args...) })
		}
		return e.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
