// Copyright 2015 Davin Hills. All rights reserved.
// MIT license. License details can be found in the LICENSE file.

package goauto

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/fsnotify.v1"
)

const (
	batchTick = 300 * time.Millisecond // batching Duration in ms
	dirOps    = fsnotify.Create | fsnotify.Rename
)

// Flags to WatchRecursive to include or ignore hidden directories
const (
	IgnoreHidden  = true
	IncludeHidden = false
)

// Flags for verbose output
const (
	Verbose = true
	Silent  = false
)

// A Pipeline watches one or more directories for changes
type Pipeline struct {
	Name       string
	Watches    []string
	Wout, Werr io.Writer
	Workflows  []Workflower
	Verbose    bool
	watcher    *fsnotify.Watcher
	events     chan []fsnotify.Event
	done       chan bool
	recDirs    map[string]bool
}

// NewPipeline returns a basic Pipeline with a dir to watch, output and error writers and a workflow
func NewPipeline(name string, verbose bool) *Pipeline {
	p := Pipeline{Name: name, Wout: os.Stdout, Werr: os.Stderr, Verbose: verbose}
	return &p
}

// Watch adds a GOPATH relative or absolute path to watch
// rejects invalid paths and ignores duplicates
func (p *Pipeline) Watch(watchDir string) (string, error) {
	d, err := AbsPath(watchDir)
	if err != nil {
		if p.Verbose {
			fmt.Fprintln(p.Wout, err)
		}
		return "", err
	}
	// Make sure we are not already watching it
	for _, w := range p.Watches {
		if w == d {
			return d, nil
		}
	}
	p.Watches = append(p.Watches, d)
	if p.watcher != nil {
		p.watcher.Add(d)
	}
	return d, nil
}

// WatchRecursive adds a GOPATH relative or absolute path to watch recursivly
func (p *Pipeline) WatchRecursive(watchDir string, ignoreHidden bool) error {
	d, err := AbsPath(watchDir)
	if err != nil {
		return err
	}
	if p.recDirs == nil {
		p.recDirs = make(map[string]bool)
	}
	p.recDirs[d] = ignoreHidden
	filepath.Walk(d, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if IsHidden(info.Name()) && ignoreHidden {
				return filepath.SkipDir
			}
			p.Watch(path)
		}
		return nil
	})
	return nil
}

// recDir checks if an event is adding or renaming a directory in a recursive watch
func (p *Pipeline) recDir(e fsnotify.Event) {
	if dirOps&e.Op != e.Op {
		return
	}
	if fi, err := os.Stat(e.Name); err != nil || !fi.IsDir() {
		return
	}

	h := IsHidden(e.Name)
	for dir, iHidden := range p.recDirs {
		if h && iHidden {
			continue
		}
		if _, err := filepath.Rel(dir, e.Name); err == nil {
			p.WatchRecursive(e.Name, iHidden)
			return
		}
	}
}

// Add adds one or more Workflows to the pipeline
func (p *Pipeline) Add(ws ...Workflower) {
	for _, w := range ws {
		p.Workflows = append(p.Workflows, w)
	}
}

// batchRun watches for file events and batches them up based on a timer
// **Thanks to github.com/egonelbre for the suggestions and examples for batch events
func (p *Pipeline) batchRun() {
	tick := time.Tick(batchTick)
	var evs []fsnotify.Event

outer:
	for {
		select {
		case event := <-p.watcher.Events:
			evs = append(evs, event)
		case <-tick:
			if len(evs) == 0 {
				continue
			}
			p.events <- evs
			evs = []fsnotify.Event{}
		case <-p.done:
			break outer
		}
	}
	close(p.done)
}

// Start begins watching for changes to files in the Watches directories
// Detected file changes will be compared with workflow regexp and if match will run the workflow tasks
func (p *Pipeline) Start() {
	if p.Wout == nil {
		p.Wout = os.Stdout
	}
	if p.Werr == nil {
		p.Werr = os.Stderr
	}
	if p.Name == "" {
		p.Name = "<UNNAMED>"
	}

	if len(p.Watches) < 1 {
		fmt.Fprintln(p.Werr, "Pipeline", p.Name, "is not watching anything")
	}

	if len(p.Workflows) < 1 {
		fmt.Fprintln(p.Werr, "Pipeline", p.Name, "has no Workflows")
	}

	// Create a watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintln(p.Werr, err)
		return
	}
	p.watcher = watcher

	// Make the channels to batch the events and signal done
	p.done = make(chan bool)
	p.events = make(chan []fsnotify.Event)

	// start watching
	go p.batchRun()

	// Add the watch directories to the watcher
	for _, w := range p.Watches {
		watcher.Add(w)
		if p.Verbose {
			fmt.Fprintf(p.Wout, "Watching %v\n", w)
		}
	}

	// block and wait to receive batched events
	for {
		select {
		case evs := <-p.events:
			for _, e := range evs {
				go p.recDir(e)
				p.queryWorkflow(e.Name, uint32(e.Op))
			}
		}
	}
}

// queryWorkflow checks for file match for each workflow and if matches executes the workflow tasks
func (p *Pipeline) queryWorkflow(fpath string, op uint32) {
	if p.Verbose {
		fmt.Fprintf(p.Wout, "Watcher event %v %v\n", fpath, op)
	}
	for _, wf := range p.Workflows {
		if wf.Match(fpath, op) {
			wf.Run(&TaskInfo{Src: fpath, Tout: p.Wout, Terr: p.Werr, Verbose: p.Verbose})
		}
	}
}

// Stop will discontinue watching for file changes
func (p *Pipeline) Stop() {
	p.done <- true
	p.watcher.Close()
}
