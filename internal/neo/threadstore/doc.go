// Package threadstore provides Neo's local Amp thread persistence backend.
//
// It stores Amp thread JSON as opaque raw blobs and exposes a small Store
// interface plus an /api/internal-compatible HTTP adapter for uploadThread,
// getThread, listThreads, and deleteThread. The package is intentionally
// standalone: it imports only the standard library and modernc.org/sqlite.
//
// Mounting example:
//
//	store, err := threadstore.OpenSQLite("/Users/me/.amp-proxy-neo/threads.db")
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer store.Close()
//
//	mux := http.NewServeMux()
//	mux.Handle("/api/internal", threadstore.NewHandler(store))
//	log.Fatal(http.ListenAndServe(":9320", mux))
//
// The handler does no authentication and no path prefix stripping. Callers are
// expected to mount it at the desired mux path and enforce any access policy
// outside this package.
package threadstore
