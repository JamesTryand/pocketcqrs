package migrations

import "sync"

var once sync.Once

// RegisterExamples adds this repo's example collection migrations to
// PocketBase's app-migrations list. Call it before the app starts — the list
// is read by RunAppMigrations during bootstrap.
//
// Not calling it leaves them unregistered rather than skipped, and that
// distinction is the whole design. The migration runner iterates only the
// registered list, so an already-applied row in _migrations for an
// unregistered migration is simply never looked at: no error, no warning,
// and no state that has to be undone. Switching the examples off and on
// again across boots is therefore symmetric, with neither direction a
// one-way door.
//
// sync.Once because core.AppMigrations is a process-global list: calling
// twice would append duplicate entries for the same migration.
func RegisterExamples() {
	once.Do(func() {
		registerTasksCollection()
		registerOrdersCollections()
	})
}
