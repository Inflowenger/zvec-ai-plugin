// Command zvec-plugin is an Inflowenger plugin node for zvec, the embedded
// vector database.
//
// It exposes collection management and document reads and writes on the
// workflow canvas — including a hybrid search that runs a vector half and a
// full-text half and fuses their rankings, which is zvec's own feature and the
// reason to reach for it over a plain vector store.
//
// zvec is embedded: there is no server, no container and no network hop. A
// "connection" is a directory on whichever host this process runs on, named by
// a settings profile and shipped with every call as body.settings — so the
// plugin itself holds no storage configuration. Collections under that
// directory are opened once and kept open, because the engine locks each one
// exclusively.
//
// The one thing it does need is the zvec C-API shared library, which is not
// Go: see the README for where to put it, or set ZVEC_LIBRARY_PATH.
package main

import (
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/Inflowenger/zvec-ai-plugin/internal/actions"
	"github.com/Inflowenger/zvec-ai-plugin/internal/zvecdb"
)

const version = "v0.1.0"

// sendTimeout is the NATS request/reply deadline. Building an index or
// embedding a batch of documents takes longer than a network round-trip, so the
// SDK's 5s default is raised here; REQ_TIMEOUT overrides it at deploy time.
const sendTimeout = 60

func main() {
	envFile := os.Getenv("INFLOW_ENV_FILE")
	if envFile == "" {
		envFile = ".env.inflow"
	}

	// The engine is loaded before anything else, so its state is reported even
	// when the platform is unreachable — but a failure is logged and carried on
	// from, not fatal. A plugin that will not start cannot serve its own forms,
	// and the forms are where an author reads what to install; every action
	// fails with the same explanation instead.
	if err := zvecdb.InitLibrary(); err != nil {
		log.Printf("zvec plugin: %v", err)
		log.Printf("zvec plugin: run `make deps` (or scripts/fetch-zvec-lib.sh) to download it")
		log.Printf("zvec plugin: serving forms, but every action will fail until the engine is in place")
	} else {
		log.Printf("zvec plugin: engine %s ready", zvecdb.Version())
	}

	// The dotenv carries the platform identity only — PLUGIN_ID, INFRA_CRED,
	// INFRA_URL. No zvec storage configuration ever lives here.
	plugin, err := sdkv1.NewPlugin(sdkv1.WithDotEnv(envFile), sdkv1.WithTimeout(sendTimeout))
	if err != nil {
		log.Fatalf("zvec plugin: cannot connect to infra (%s): %v", envFile, err)
	}

	registry := actions.New()
	defer registry.Close()

	plugin.Intro(sdkv1.PluginIntro{
		Name:     "ZVEC",
		Author:   "Inflowenger",
		Version:  version,
		Settings: registry.SettingsForm(),
		Manual:   manual(),
	})
	plugin.RequiredParams(registry.Settings())

	all := registry.All()
	plugin.AddAction(all...)
	plugin.AddMeta(registry.Metas()...)

	if err := plugin.Start(); err != nil {
		log.Fatalf("zvec plugin: start: %v", err)
	}

	methods := make([]string, 0, len(all))
	for _, action := range all {
		methods = append(methods, action.Method)
	}
	log.Printf("zvec plugin %s ready with %d actions: %s", version, len(all), strings.Join(methods, ", "))
	log.Printf("zvec plugin: each call brings its own data directory in body.settings — bind a settings profile to the node")

	// Start() only wires up subscriptions; the process has to stay alive to
	// serve them.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	// Collections are flushed and closed before the engine goes away, so a
	// restart does not have to recover writes that were only in memory, and no
	// collection is left holding its lock.
	log.Println("zvec plugin: closing collections")
	registry.Close()
	if err := zvecdb.ShutdownLibrary(); err != nil {
		log.Printf("zvec plugin: shutdown: %v", err)
	}
	log.Println("zvec plugin: shutting down")
}
