package functions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dop251/goja"
)

// LoadDir loads functions from dir (PocketBase pb_hooks precedent).
//
// Files ending in .js declare their triggers via directives in the first
// lines:
//
//	//@trigger event TaskCreated TaskCompleted   -> durable event function
//	//@trigger http                              -> served at /api/fn/{basename}
//
// A file may carry both. Files without directives are ignored (logged).
// A missing dir is not an error: functions are optional.
func LoadDir(rt *GojaRuntime, dir string) (*HTTPRegistry, error) {
	httpReg := NewHTTPRegistry(rt)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return httpReg, nil
		}
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		src := string(raw)
		eventTypes, isHTTP := parseTriggers(src)

		if len(eventTypes) == 0 && !isHTTP {
			rt.logger("function file has no //@trigger directive, ignored", "file", path)
			continue
		}

		if len(eventTypes) > 0 {
			if err := rt.RegisterEventFunction(eventTypes, entry.Name(), src); err != nil {
				return nil, err
			}
		}
		if isHTTP {
			prog, err := goja.Compile(entry.Name(), src, false)
			if err != nil {
				return nil, fmt.Errorf("functions: compile %s: %w", entry.Name(), err)
			}
			name := strings.TrimSuffix(entry.Name(), ".js")
			httpReg.register(name, prog)
			rt.logger("HTTP function registered", "name", name, "path", "/api/fn/"+name)
		}
	}

	return httpReg, nil
}

// parseTriggers scans the leading comment lines for //@trigger directives.
func parseTriggers(src string) (eventTypes []string, isHTTP bool) {
	for line := range strings.Lines(src) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "//") {
			break // directives must lead the file
		}
		rest, ok := strings.CutPrefix(line, "//@trigger")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "event":
			eventTypes = append(eventTypes, fields[1:]...)
		case "http":
			isHTTP = true
		}
	}
	return eventTypes, isHTTP
}
