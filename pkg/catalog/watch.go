package catalog

import (
	"log"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch starts a file watcher on catalogPath and calls onChange whenever the
// file is written or replaced (e.g. by git-sync, picoclaw-scan update, or a
// manual copy).  The returned stop function cancels the watcher.
func Watch(catalogPath string, onChange func(*Catalog)) (stop func(), err error) {
	expanded := expandHome(catalogPath)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	// Watch the directory so we catch atomic renames (tmp → real).
	if err := watcher.Add(filepath.Dir(expanded)); err != nil {
		watcher.Close()
		return nil, err
	}

	go func() {
		debounce := time.NewTimer(0)
		<-debounce.C

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Name != expanded && filepath.Base(event.Name) != filepath.Base(expanded) {
					continue
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
					debounce.Reset(300 * time.Millisecond)
				}
			case watchErr, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("catalog watcher error: %v", watchErr)
			case <-debounce.C:
				cat, err := Load(catalogPath)
				if err != nil {
					log.Printf("catalog reload error: %v", err)
					continue
				}
				onChange(cat)
			}
		}
	}()

	return func() { watcher.Close() }, nil
}
