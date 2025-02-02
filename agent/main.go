package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/up9inc/mizu/shared/kubernetes"
	"io/ioutil"
	v1 "k8s.io/api/core/v1"
	"mizuserver/pkg/api"
	"mizuserver/pkg/config"
	"mizuserver/pkg/controllers"
	"mizuserver/pkg/database"
	"mizuserver/pkg/models"
	"mizuserver/pkg/providers"
	"mizuserver/pkg/routes"
	"mizuserver/pkg/up9"
	"mizuserver/pkg/utils"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"plugin"
	"sort"
	"syscall"
	"time"

	"github.com/gin-contrib/static"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/op/go-logging"
	"github.com/up9inc/mizu/shared"
	"github.com/up9inc/mizu/shared/logger"
	"github.com/up9inc/mizu/tap"
	tapApi "github.com/up9inc/mizu/tap/api"
)

var tapperMode = flag.Bool("tap", false, "Run in tapper mode without API")
var apiServerMode = flag.Bool("api-server", false, "Run in API server mode with API")
var standaloneMode = flag.Bool("standalone", false, "Run in standalone tapper and API mode")
var apiServerAddress = flag.String("api-server-address", "", "Address of mizu API server")
var namespace = flag.String("namespace", "", "Resolve IPs if they belong to resources in this namespace (default is all)")
var harsReaderMode = flag.Bool("hars-read", false, "Run in hars-read mode")
var harsDir = flag.String("hars-dir", "", "Directory to read hars from")

var extensions []*tapApi.Extension             // global
var extensionsMap map[string]*tapApi.Extension // global

const (
	socketConnectionRetries = 10
	socketConnectionRetryDelay = time.Second * 2
	socketHandshakeTimeout = time.Second * 2
)

func main() {
	logLevel := determineLogLevel()
	logger.InitLoggerStderrOnly(logLevel)
	flag.Parse()
	if err := config.LoadConfig(); err != nil {
		logger.Log.Fatalf("Error loading config file %v", err)
	}
	loadExtensions()

	if !*tapperMode && !*apiServerMode && !*standaloneMode && !*harsReaderMode {
		panic("One of the flags --tap, --api or --standalone or --hars-read must be provided")
	}

	if *standaloneMode {
		api.StartResolving(*namespace)

		outputItemsChannel := make(chan *tapApi.OutputChannelItem)
		filteredOutputItemsChannel := make(chan *tapApi.OutputChannelItem)

		filteringOptions := getTrafficFilteringOptions()
		hostMode := os.Getenv(shared.HostModeEnvVar) == "1"
		tapOpts := &tap.TapOpts{HostMode: hostMode}
		tap.StartPassiveTapper(tapOpts, outputItemsChannel, extensions, filteringOptions)

		go filterItems(outputItemsChannel, filteredOutputItemsChannel)
		go api.StartReadingEntries(filteredOutputItemsChannel, nil, extensionsMap)

		hostApi(nil)
	} else if *tapperMode {
		logger.Log.Infof("Starting tapper, websocket address: %s", *apiServerAddress)
		if *apiServerAddress == "" {
			panic("API server address must be provided with --api-server-address when using --tap")
		}

		tapTargets := getTapTargets()
		if tapTargets != nil {
			tap.SetFilterAuthorities(tapTargets)
			logger.Log.Infof("Filtering for the following authorities: %v", tap.GetFilterIPs())
		}

		filteredOutputItemsChannel := make(chan *tapApi.OutputChannelItem)

		filteringOptions := getTrafficFilteringOptions()
		hostMode := os.Getenv(shared.HostModeEnvVar) == "1"
		tapOpts := &tap.TapOpts{HostMode: hostMode}
		tap.StartPassiveTapper(tapOpts, filteredOutputItemsChannel, extensions, filteringOptions)
		socketConnection, err := dialSocketWithRetry(*apiServerAddress, socketConnectionRetries, socketConnectionRetryDelay)
		if err != nil {
			panic(fmt.Sprintf("Error connecting to socket server at %s %v", *apiServerAddress, err))
		}
		logger.Log.Infof("Connected successfully to websocket %s", *apiServerAddress)

		go pipeTapChannelToSocket(socketConnection, filteredOutputItemsChannel)
	} else if *apiServerMode {
		database.InitDataBase(config.Config.AgentDatabasePath)
		api.StartResolving(*namespace)

		outputItemsChannel := make(chan *tapApi.OutputChannelItem)
		filteredOutputItemsChannel := make(chan *tapApi.OutputChannelItem)

		go filterItems(outputItemsChannel, filteredOutputItemsChannel)
		go api.StartReadingEntries(filteredOutputItemsChannel, nil, extensionsMap)

		syncEntriesConfig := getSyncEntriesConfig()
		if syncEntriesConfig != nil {
			if err := up9.SyncEntries(syncEntriesConfig); err != nil {
				panic(fmt.Sprintf("Error syncing entries, err: %v", err))
			}
		}

		hostApi(outputItemsChannel)
	} else if *harsReaderMode {
		outputItemsChannel := make(chan *tapApi.OutputChannelItem, 1000)
		filteredHarChannel := make(chan *tapApi.OutputChannelItem)

		go filterItems(outputItemsChannel, filteredHarChannel)
		go api.StartReadingEntries(filteredHarChannel, harsDir, extensionsMap)
		hostApi(nil)
	}

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)
	<-signalChan

	logger.Log.Info("Exiting")
}

func loadExtensions() {
	dir, _ := filepath.Abs(filepath.Dir(os.Args[0]))
	extensionsDir := path.Join(dir, "./extensions/")

	files, err := ioutil.ReadDir(extensionsDir)
	if err != nil {
		logger.Log.Fatal(err)
	}
	extensions = make([]*tapApi.Extension, len(files))
	extensionsMap = make(map[string]*tapApi.Extension)
	for i, file := range files {
		filename := file.Name()
		logger.Log.Infof("Loading extension: %s\n", filename)
		extension := &tapApi.Extension{
			Path: path.Join(extensionsDir, filename),
		}
		plug, _ := plugin.Open(extension.Path)
		extension.Plug = plug
		symDissector, err := plug.Lookup("Dissector")

		var dissector tapApi.Dissector
		var ok bool
		dissector, ok = symDissector.(tapApi.Dissector)
		if err != nil || !ok {
			panic(fmt.Sprintf("Failed to load the extension: %s\n", extension.Path))
		}
		dissector.Register(extension)
		extension.Dissector = dissector
		extensions[i] = extension
		extensionsMap[extension.Protocol.Name] = extension
	}

	sort.Slice(extensions, func(i, j int) bool {
		return extensions[i].Protocol.Priority < extensions[j].Protocol.Priority
	})

	for _, extension := range extensions {
		logger.Log.Infof("Extension Properties: %+v\n", extension)
	}

	controllers.InitExtensionsMap(extensionsMap)
}

func hostApi(socketHarOutputChannel chan<- *tapApi.OutputChannelItem) {
	app := gin.Default()

	app.GET("/echo", func(c *gin.Context) {
		c.String(http.StatusOK, "Here is Mizu agent")
	})

	eventHandlers := api.RoutesEventHandlers{
		SocketOutChannel: socketHarOutputChannel,
	}

	app.Use(DisableRootStaticCache())
	app.Use(static.ServeRoot("/", "./site"))
	app.Use(CORSMiddleware()) // This has to be called after the static middleware, does not work if its called before

	api.WebSocketRoutes(app, &eventHandlers)
	routes.EntriesRoutes(app)
	routes.MetadataRoutes(app)
	routes.StatusRoutes(app)
	routes.NotFoundRoute(app)

	if config.Config.DaemonMode {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if _, err := startMizuTapperSyncer(ctx); err != nil {
			logger.Log.Fatalf("error initializing tapper syncer: %+v", err)
		}
	}

	utils.StartServer(app)
}

func DisableRootStaticCache() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.RequestURI == "/" {
			// Disable cache only for the main static route
			c.Writer.Header().Set("Cache-Control", "no-store")
		}

		c.Next()
	}
}

func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

func parseEnvVar(env string) map[string][]string {
	var mapOfList map[string][]string

	val, present := os.LookupEnv(env)

	if !present {
		return mapOfList
	}

	err := json.Unmarshal([]byte(val), &mapOfList)
	if err != nil {
		panic(fmt.Sprintf("env var %s's value of %s is invalid! must be map[string][]string %v", env, mapOfList, err))
	}
	return mapOfList
}

func getTapTargets() []string {
	nodeName := os.Getenv(shared.NodeNameEnvVar)
	tappedAddressesPerNodeDict := parseEnvVar(shared.TappedAddressesPerNodeDictEnvVar)
	return tappedAddressesPerNodeDict[nodeName]
}

func getTrafficFilteringOptions() *tapApi.TrafficFilteringOptions {
	filteringOptionsJson := os.Getenv(shared.MizuFilteringOptionsEnvVar)
	if filteringOptionsJson == "" {
		return &tapApi.TrafficFilteringOptions{
			IgnoredUserAgents: []string{},
		}
	}
	var filteringOptions tapApi.TrafficFilteringOptions
	err := json.Unmarshal([]byte(filteringOptionsJson), &filteringOptions)
	if err != nil {
		panic(fmt.Sprintf("env var %s's value of %s is invalid! json must match the api.TrafficFilteringOptions struct %v", shared.MizuFilteringOptionsEnvVar, filteringOptionsJson, err))
	}

	return &filteringOptions
}

func filterItems(inChannel <-chan *tapApi.OutputChannelItem, outChannel chan *tapApi.OutputChannelItem) {
	for message := range inChannel {
		if message.ConnectionInfo.IsOutgoing && api.CheckIsServiceIP(message.ConnectionInfo.ServerIP) {
			continue
		}

		outChannel <- message
	}
}

func pipeTapChannelToSocket(connection *websocket.Conn, messageDataChannel <-chan *tapApi.OutputChannelItem) {
	if connection == nil {
		panic("Websocket connection is nil")
	}

	if messageDataChannel == nil {
		panic("Channel of captured messages is nil")
	}

	for messageData := range messageDataChannel {
		marshaledData, err := models.CreateWebsocketTappedEntryMessage(messageData)
		if err != nil {
			logger.Log.Errorf("error converting message to json %v, err: %s, (%v,%+v)", messageData, err, err, err)
			continue
		}

		// NOTE: This is where the `*tapApi.OutputChannelItem` leaves the code
		// and goes into the intermediate WebSocket.
		err = connection.WriteMessage(websocket.TextMessage, marshaledData)
		if err != nil {
			logger.Log.Errorf("error sending message through socket server %v, err: %s, (%v,%+v)", messageData, err, err, err)
			if errors.Is(err, syscall.EPIPE) {
				logger.Log.Warning("detected socket disconnection, reestablishing socket connection")
				connection, err = dialSocketWithRetry(*apiServerAddress, socketConnectionRetries, socketConnectionRetryDelay)
				if err != nil {
					logger.Log.Fatalf("error reestablishing socket connection: %v", err)
				} else {
					logger.Log.Info("recovered connection successfully")
				}
			}
			continue
		}
	}
}

func getSyncEntriesConfig() *shared.SyncEntriesConfig {
	syncEntriesConfigJson := os.Getenv(shared.SyncEntriesConfigEnvVar)
	if syncEntriesConfigJson == "" {
		return nil
	}

	var syncEntriesConfig = &shared.SyncEntriesConfig{}
	err := json.Unmarshal([]byte(syncEntriesConfigJson), syncEntriesConfig)
	if err != nil {
		panic(fmt.Sprintf("env var %s's value of %s is invalid! json must match the shared.SyncEntriesConfig struct, err: %v", shared.SyncEntriesConfigEnvVar, syncEntriesConfigJson, err))
	}

	return syncEntriesConfig
}

func determineLogLevel() (logLevel logging.Level) {
	logLevel = logging.INFO
	if os.Getenv(shared.DebugModeEnvVar) == "1" {
		logLevel = logging.DEBUG
	}
	return
}

func dialSocketWithRetry(socketAddress string, retryAmount int, retryDelay time.Duration) (*websocket.Conn, error) {
	var lastErr error
	dialer := &websocket.Dialer{ // we use our own dialer instead of the default due to the default's 45 sec handshake timeout, we occasionally encounter hanging socket handshakes when tapper tries to connect to api too soon
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: socketHandshakeTimeout,
	}
	for i := 1; i < retryAmount; i++ {
		socketConnection, _, err := dialer.Dial(socketAddress, nil)
		if err != nil {
			if i < retryAmount {
				logger.Log.Infof("socket connection to %s failed: %v, retrying %d out of %d in %d seconds...", socketAddress, err, i, retryAmount, retryDelay / time.Second)
				time.Sleep(retryDelay)
			}
		} else {
			return socketConnection, nil
		}
	}
	return nil, lastErr
}


func startMizuTapperSyncer(ctx context.Context) (*kubernetes.MizuTapperSyncer, error){
	provider, err := kubernetes.NewProviderInCluster()
	if err != nil {
		return nil, err
	}

	tapperSyncer, err := kubernetes.CreateAndStartMizuTapperSyncer(ctx, provider, kubernetes.TapperSyncerConfig{
		TargetNamespaces:         config.Config.TargetNamespaces,
		PodFilterRegex:           config.Config.TapTargetRegex.Regexp,
		MizuResourcesNamespace:   config.Config.MizuResourcesNamespace,
		AgentImage:               config.Config.AgentImage,
		TapperResources:          config.Config.TapperResources,
		ImagePullPolicy:          v1.PullPolicy(config.Config.PullPolicy),
		DumpLogs:                 config.Config.DumpLogs,
		IgnoredUserAgents:        config.Config.IgnoredUserAgents,
		MizuApiFilteringOptions:  config.Config.MizuApiFilteringOptions,
		MizuServiceAccountExists: true, //assume service account exists since daemon mode will not function without it anyway
	})

	if err != nil {
		return nil, err
	}

	// handle tapperSyncer events (pod changes and errors)
	go func() {
		for {
			select {
			case syncerErr, ok := <-tapperSyncer.ErrorOut:
				if !ok {
					logger.Log.Debug("mizuTapperSyncer err channel closed, ending listener loop")
					return
				}
				logger.Log.Fatalf("fatal tap syncer error: %v", syncerErr)
			case _, ok := <-tapperSyncer.TapPodChangesOut:
				if !ok {
					logger.Log.Debug("mizuTapperSyncer pod changes channel closed, ending listener loop")
					return
				}
				tapStatus := shared.TapStatus{Pods: kubernetes.GetPodInfosForPods(tapperSyncer.CurrentlyTappedPods)}

				serializedTapStatus, err := json.Marshal(shared.CreateWebSocketStatusMessage(tapStatus))
				if err != nil {
					logger.Log.Fatalf("error serializing tap status: %v", err)
				}
				api.BroadcastToBrowserClients(serializedTapStatus)
				providers.TapStatus.Pods = tapStatus.Pods
			case <-ctx.Done():
				logger.Log.Debug("mizuTapperSyncer event listener loop exiting due to context done")
				return
			}
		}
	}()

	return tapperSyncer, nil
}
