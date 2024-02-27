package commands

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/alecthomas/kong"
	"github.com/facundoolano/jorge/config"
	"github.com/facundoolano/jorge/site"
	"github.com/fsnotify/fsnotify"
)

type Serve struct {
	ProjectDir string `arg:"" name:"path" optional:"" default:"." help:"Path to the website project to serve."`
	Host       string `short:"h" default:"localhost" help:"Host to run the server on."`
	Port       int    `short:"p" default:"4001" help:"Port to run the server on."`
	NoReload   bool   `help:"Disable live reloading."`
}

func (cmd *Serve) Run(ctx *kong.Context) error {
	config, err := config.LoadDev(cmd.ProjectDir, cmd.Host, cmd.Port, !cmd.NoReload)
	if err != nil {
		return err
	}

	if _, err := os.Stat(config.SrcDir); os.IsNotExist(err) {
		return fmt.Errorf("missing src directory")
	}

	// watch for changes in src and layouts, and trigger a rebuild
	watcher, broker, err := setupWatcher(config)
	if err != nil {
		return err
	}
	defer watcher.Close()

	// serve the target dir with a file server
	fs := http.FileServer(HTMLFileSystem{http.Dir(config.TargetDir)})
	http.Handle("/", http.StripPrefix("/", fs))

	if config.LiveReload {
		// handle client requests to listen to server-sent events
		http.Handle("/_events/", makeServerEventsHandler(broker))
	}

	addr := fmt.Sprintf("%s:%d", config.ServerHost, config.ServerPort)
	return http.ListenAndServe(addr, nil)
}

// Return an http.HandlerFunc that establishes a server-sent event stream with clients,
// subscribes to site rebuild events received through the given event broker
// and forwards them to the client.
func makeServerEventsHandler(broker *EventBroker) http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		res.Header().Set("Content-Type", "text/event-stream")
		res.Header().Set("Connection", "keep-alive")
		res.Header().Set("Cache-Control", "no-cache")
		res.Header().Set("Access-Control-Allow-Origin", "*")

		id, events := broker.subscribe()
		for {
			select {
			case <-events:
				// send an event to the connected client.
				// data\n\n just means send an empty, unnamed event
				// since we only need to support the single reload operation.
				fmt.Fprint(res, "data\n\n")
				res.(http.Flusher).Flush()
			case <-req.Context().Done():
				broker.unsubscribe(id)
				return
			}
		}
	}
}

// Sets up a watcher that will publish changes in the site source files
// to the returned event broker.
func setupWatcher(config *config.Config) (*fsnotify.Watcher, *EventBroker, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}

	broker := newEventBroker()

	// the rebuild is handled after some delay to prevent bursts of events to trigger repeated rebuilds
	// which can cause the browser to refresh while another unfinished build is in progress (refreshing to
	// a missing file). The initial build is done immediately.
	rebuildAfter := time.AfterFunc(0, func() {
		rebuildSite(config, watcher, broker)
	})

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				// chmod events are noisy, ignore them
				if event.Has(fsnotify.Chmod) {
					continue
				}

				// Schedule a rebuild to trigger after a delay. If there was another one pending
				// it will be canceled.
				fmt.Printf("\nfile %s changed\n", event.Name)
				rebuildAfter.Stop()
				rebuildAfter.Reset(100 * time.Millisecond)

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				fmt.Println("error:", err)
			}
		}
	}()

	err = addAll(watcher, config)

	return watcher, broker, err
}

// Add the layouts and all source directories to the given watcher
func addAll(watcher *fsnotify.Watcher, config *config.Config) error {
	err := watcher.Add(config.LayoutsDir)
	err = watcher.Add(config.DataDir)
	err = watcher.Add(config.IncludesDir)
	// fsnotify watches all files within a dir, but non recursively
	// this walks through the src dir and adds watches for each found directory
	filepath.WalkDir(config.SrcDir, func(path string, entry fs.DirEntry, err error) error {
		if entry.IsDir() {
			watcher.Add(path)
		}
		return nil
	})
	return err
}

func rebuildSite(config *config.Config, watcher *fsnotify.Watcher, broker *EventBroker) {
	fmt.Printf("building site\n")

	// since new nested directories could be triggering this change, and we need to watch those too
	// and since re-watching files is a noop, I just re-add the entire src everytime there's a change
	if err := addAll(watcher, config); err != nil {
		fmt.Println("couldn't add watchers:", err)
	}

	site, err := site.Load(*config)
	if err != nil {
		fmt.Println("load error:", err)
		return
	}

	if err := site.Build(); err != nil {
		fmt.Println("build error:", err)
		return
	}

	broker.publish("rebuild")

	fmt.Println("done\nserving at", config.SiteUrl)
}

// Tweaks the http file system to construct a server that hides the .html suffix from requests.
// Based on https://stackoverflow.com/a/57281956/993769
type HTMLFileSystem struct {
	d http.Dir
}

func (d HTMLFileSystem) Open(name string) (http.File, error) {
	// Try name as supplied
	f, err := d.d.Open(name)
	if os.IsNotExist(err) {
		// Not found, try with .html
		if f, err := d.d.Open(name + ".html"); err == nil {
			return f, nil
		}
	}
	return f, err
}

// The event broker allows the file watcher to publish site rebuild events
// and register http clients to listen for them, in order to trigger browser refresh
// events after the the site has been rebuilt.
type EventBroker struct {
	inEvents        chan string
	inSubscriptions chan Subscription
	subscribers     map[uint64]chan string
	idgen           atomic.Uint64
}

type Subscription struct {
	id        uint64
	outEvents chan string
}

func newEventBroker() *EventBroker {
	broker := EventBroker{
		inEvents:        make(chan string),
		inSubscriptions: make(chan Subscription),
		subscribers:     map[uint64]chan string{},
	}

	go func() {
		for {
			select {
			case msg := <-broker.inSubscriptions:
				if msg.outEvents != nil {
					// subscribe
					broker.subscribers[msg.id] = msg.outEvents
				} else {
					// unsubscribe
					close(broker.subscribers[msg.id])
					delete(broker.subscribers, msg.id)
				}
			case msg := <-broker.inEvents:
				// send the event to all the subscribers
				for _, outEvents := range broker.subscribers {
					outEvents <- msg
				}
			}
		}
	}()
	return &broker
}

// Adds a subscription to this broker events, returning a subscriber id
// (useful for unsubscribing later) and a channel where events will be delivered.
func (broker *EventBroker) subscribe() (uint64, <-chan string) {
	id := broker.idgen.Add(1)
	outEvents := make(chan string)
	broker.inSubscriptions <- Subscription{id, outEvents}
	return id, outEvents
}

// Remove the subscriber with the given id from the broker,
// closing its associated channel.
func (broker *EventBroker) unsubscribe(id uint64) {
	broker.inSubscriptions <- Subscription{id: id, outEvents: nil}
}

// Publish an event to all the broker subscribers.
func (broker *EventBroker) publish(event string) {
	broker.inEvents <- event
}
