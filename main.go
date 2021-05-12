// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2021 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	flag "maunium.net/go/mauflag"
	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/config"
	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/database/upgrades"
	"go.mau.fi/mautrix-imessage/imessage"
	_ "go.mau.fi/mautrix-imessage/imessage/ios"
	_ "go.mau.fi/mautrix-imessage/imessage/mac"
	"go.mau.fi/mautrix-imessage/ipc"
)

var (
	// These are static
	Name = "mautrix-imessage"
	URL  = "https://github.com/tulir/mautrix-imessage"
	// This is changed when making a release
	Version = "0.1.0"
	// These are filled at build time with the -X linker flag
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"

	VersionString = ""
)

func init() {
	if len(Tag) > 0 && Tag[0] == 'v' {
		Tag = Tag[1:]
	}
	if Tag != Version && !strings.HasSuffix(Version, "+dev") {
		Version += "+dev"
	}
	if Tag == Version {
		VersionString = fmt.Sprintf("%s %s (%s)", Name, Tag, BuildTime)
	} else if len(Commit) > 8 {
		VersionString = fmt.Sprintf("%s %s.%s (%s)", Name, Version, Commit[:8], BuildTime)
	} else {
		VersionString = fmt.Sprintf("%s %s.unknown", Name, Version)
	}

	mautrix.DefaultUserAgent = fmt.Sprintf("mautrix-imessage/%s %s", Version, mautrix.DefaultUserAgent)
}

var configPath = flag.MakeFull("c", "config", "The path to your config file.", "config.yaml").String()
var configURL = flag.MakeFull("u", "url", "The URL to download the config file from.", "").String()
var configOutputRedirect = flag.MakeFull("o", "output-redirect", "Whether or not to output the URL of the first redirect when downloading the config file.", "false").Bool()
//var baseConfigPath = flag.MakeFull("b", "base-config", "The path to the example config file.", "example-config.yaml").String()
var registrationPath = flag.MakeFull("r", "registration", "The path where to save the appservice registration.", "registration.yaml").String()
var generateRegistration = flag.MakeFull("g", "generate-registration", "Generate registration and quit.", "false").Bool()
var version = flag.MakeFull("v", "version", "View bridge version and quit.", "false").Bool()
var ignoreUnsupportedDatabase = flag.Make().LongKey("ignore-unsupported-database").Usage("Run even if database is too new").Default("false").Bool()
var wantHelp, _ = flag.MakeHelpFlag()

func (bridge *Bridge) GenerateRegistration() {
	reg, err := bridge.Config.NewRegistration()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Failed to generate registration:", err)
		os.Exit(20)
	}

	err = reg.Save(*registrationPath)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Failed to save registration:", err)
		os.Exit(21)
	}

	err = bridge.Config.Save(*configPath)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Failed to save config:", err)
		os.Exit(22)
	}
	fmt.Println("Registration generated. Add the path to the registration to your Synapse config, restart it, then start the bridge.")
	os.Exit(0)
}

type Bridge struct {
	AS             *appservice.AppService
	EventProcessor *appservice.EventProcessor
	MatrixHandler  *MatrixHandler
	Config         *config.Config
	DB             *database.Database
	Log            log.Logger
	StateStore     *database.SQLStateStore
	Bot            *appservice.IntentAPI
	Crypto         Crypto
	IM             imessage.API
	IMHandler      *iMessageHandler
	IPC            *ipc.Processor

	user          *User
	portalsByMXID map[id.RoomID]*Portal
	portalsByGUID map[string]*Portal
	portalsLock   sync.Mutex
	puppets       map[string]*Puppet
	puppetsLock   sync.Mutex
	stopping      bool
	stop          chan struct{}
}

type Crypto interface {
	HandleMemberEvent(*event.Event)
	Decrypt(*event.Event) (*event.Event, error)
	Encrypt(id.RoomID, event.Type, event.Content) (*event.EncryptedEventContent, error)
	WaitForSession(id.RoomID, id.SenderKey, id.SessionID, time.Duration) bool
	ResetSession(id.RoomID)
	Reset()
	Init() error
	Start()
	Stop()
}

func NewBridge() *Bridge {
	bridge := &Bridge{
		portalsByMXID: make(map[id.RoomID]*Portal),
		portalsByGUID: make(map[string]*Portal),
		puppets:       make(map[string]*Puppet),
		stop:          make(chan struct{}, 1),
	}

	var err error
	bridge.Config, err = config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Failed to load config:", err)
		os.Exit(10)
	}
	return bridge
}

func (bridge *Bridge) ensureConnection() {
	for {
		resp, err := bridge.Bot.Whoami()
		if err != nil {
			if httpErr, ok := err.(mautrix.HTTPError); ok && httpErr.RespError != nil && httpErr.RespError.ErrCode == "M_UNKNOWN_ACCESS_TOKEN" {
				bridge.Log.Fatalln("Access token invalid. Is the registration installed in your homeserver correctly?")
				os.Exit(16)
			}
			bridge.Log.Errorfln("Failed to connect to homeserver: %v. Retrying in 10 seconds...", err)
			time.Sleep(10 * time.Second)
		} else if resp.UserID != bridge.Bot.UserID {
			bridge.Log.Fatalln("Unexpected user ID in whoami call: got %s, expected %s", resp.UserID, bridge.Bot.UserID)
			os.Exit(17)
		} else {
			break
		}
	}
}

func (bridge *Bridge) Init() {
	var err error

	bridge.AS, err = bridge.Config.MakeAppService()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Failed to initialize AppService:", err)
		os.Exit(11)
	}
	bridge.AS.PrepareWebsocket()
	_, _ = bridge.AS.Init()
	bridge.Bot = bridge.AS.BotIntent()

	bridge.Log = log.Create()
	bridge.Config.Logging.Configure(bridge.Log)
	log.DefaultLogger = bridge.Log.(*log.BasicLogger)
	if len(bridge.Config.Logging.FileNameFormat) > 0 {
		err = log.OpenFile()
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "Failed to open log file:", err)
			os.Exit(12)
		}
	}
	bridge.Log.Infoln("Initializing", VersionString)
	bridge.AS.Log = log.Sub("Matrix")

	bridge.Log.Debugln("Initializing database connection")
	bridge.DB, err = database.New("sqlite3", bridge.Config.AppService.Database, bridge.Log)
	if err != nil {
		bridge.Log.Fatalln("Failed to initialize database connection:", err)
		os.Exit(13)
	}

	bridge.Log.Debugln("Initializing state store")
	bridge.StateStore = database.NewSQLStateStore(bridge.DB)
	bridge.AS.StateStore = bridge.StateStore

	bridge.Log.Debugln("Initializing Matrix event processor")
	bridge.EventProcessor = appservice.NewEventProcessor(bridge.AS)
	bridge.Log.Debugln("Initializing Matrix event handler")
	bridge.MatrixHandler = NewMatrixHandler(bridge)

	bridge.IPC = ipc.NewProcessor(bridge.Log)
	bridge.IPC.SetHandler("reset-encryption", bridge.ipcResetEncryption)
	bridge.IPC.SetHandler("ping", bridge.ipcPing)
	bridge.IPC.SetHandler("stop", bridge.ipcStop)

	bridge.Log.Debugln("Initializing iMessage connector")
	bridge.IM, err = imessage.NewAPI(bridge)
	if err != nil {
		bridge.Log.Fatalln("Failed to initialize iMessage connector:", err)
		os.Exit(14)
	}

	bridge.IMHandler = NewiMessageHandler(bridge)
	bridge.Crypto = NewCryptoHelper(bridge)
}

type PingResponse struct {
	OK bool `json:"ok"`
}

func (bridge *Bridge) GetIPC() *ipc.Processor {
	return bridge.IPC
}

func (bridge *Bridge) GetLog() log.Logger {
	return bridge.Log
}

func (bridge *Bridge) GetConnectorConfig() *imessage.PlatformConfig {
	return bridge.Config.IMessage
}

func (bridge *Bridge) ipcResetEncryption(_ json.RawMessage) interface{} {
	bridge.Crypto.Reset()
	return PingResponse{true}
}

func (bridge *Bridge) ipcPing(_ json.RawMessage) interface{} {
	return PingResponse{true}
}

const defaultReconnectBackoff = 2 * time.Second
const maxReconnectBackoff = 2 * time.Minute
const reconnectBackoffReset = 5 * time.Minute

func (bridge *Bridge) startWebsocket() {
	onConnect := func() {
		go bridge.MatrixHandler.SendBridgeStatus()
	}
	reconnectBackoff := defaultReconnectBackoff
	lastDisconnect := time.Now().UnixNano()
	for {
		err := bridge.AS.StartWebsocket(bridge.Config.Homeserver.WSProxy, onConnect)
		if err == appservice.WebsocketManualStop {
			return
		} else if closeCommand := (&appservice.CloseCommand{}); errors.As(err, &closeCommand) && closeCommand.Status == appservice.MeowConnectionReplaced {
			bridge.Log.Infoln("Appservice websocket closed by another instance of the bridge, shutting down...")
			bridge.Stop()
			return
		} else if err != nil {
			bridge.Log.Errorln("Error in appservice websocket:", err)
		}
		if bridge.stopping {
			return
		}
		now := time.Now().UnixNano()
		if lastDisconnect + reconnectBackoffReset.Nanoseconds() < now {
			reconnectBackoff = defaultReconnectBackoff
		} else {
			reconnectBackoff *= 2
			if reconnectBackoff > maxReconnectBackoff {
				reconnectBackoff = maxReconnectBackoff
			}
		}
		lastDisconnect = now
		bridge.Log.Infofln("Websocket disconnected, reconnecting in %d seconds...", reconnectBackoff.Seconds())
		time.Sleep(reconnectBackoff)
	}
}

func (bridge *Bridge) Start() {
	bridge.Log.Debugln("Running database upgrades")
	err := bridge.DB.Init()
	if err != nil && (err != upgrades.UnsupportedDatabaseVersion || !*ignoreUnsupportedDatabase) {
		bridge.Log.Fatalln("Failed to initialize database:", err)
		os.Exit(15)
	}
	bridge.Log.Debugln("Checking connection to homeserver")
	bridge.ensureConnection()
	if bridge.Crypto != nil {
		err := bridge.Crypto.Init()
		if err != nil {
			bridge.Log.Fatalln("Error initializing end-to-bridge encryption:", err)
			os.Exit(19)
		}
	}
	bridge.Log.Debugln("Finding bridge user")
	bridge.user = bridge.loadDBUser()
	bridge.user.initDoublePuppet()
	bridge.Log.Debugln("Connecting to iMessage")
	go func() {
		err := bridge.IM.Start()
		if err != nil {
			bridge.Log.Fatalln("Error in iMessage connection:", err)
			os.Exit(40)
		}
	}()
	bridge.Log.Debugln("Starting application service websocket")
	go bridge.startWebsocket()
	bridge.Log.Debugln("Starting event processor")
	go bridge.EventProcessor.Start()
	go bridge.MatrixHandler.HandleWebsocketCommands()
	bridge.Log.Debugln("Stating iMessage handler")
	go bridge.IMHandler.Start()
	bridge.Log.Debugln("Starting IPC loop")
	go bridge.IPC.Loop()
	go bridge.UpdateBotProfile()
	if bridge.Crypto != nil {
		go bridge.Crypto.Start()
	}

	go bridge.StartupSync()
	bridge.Log.Infoln("Initialization complete")
	go bridge.PeriodicSync()
}

func (bridge *Bridge) StartupSync() {
	alreadySynced := make(map[string]bool)
	for _, portal := range bridge.GetAllPortals() {
		removed := portal.CleanupIfEmpty(true)
		if !removed && len(portal.MXID) > 0 {
			portal.Sync(true)
			alreadySynced[portal.GUID] = true
		}
	}
	syncChatMaxAge := time.Duration(bridge.Config.Bridge.ChatSyncMaxAge*24*60) * time.Minute
	chats, err := bridge.IM.GetChatsWithMessagesAfter(time.Now().Add(-syncChatMaxAge))
	if err != nil {
		bridge.Log.Errorln("Failed to get chat list to backfill:", err)
		return
	}
	for _, chatID := range chats {
		if _, isSynced := alreadySynced[chatID]; !isSynced {
			portal := bridge.GetPortalByGUID(chatID)
			portal.Sync(true)
		}
	}
	bridge.Log.Infoln("Startup sync complete")
}

func (bridge *Bridge) PeriodicSync() {
	for {
		time.Sleep(time.Hour)
		bridge.Log.Infoln("Executing periodic chat/contact info sync")
		for _, portal := range bridge.GetAllPortals() {
			if len(portal.MXID) > 0 {
				portal.Sync(false)
			}
		}
	}
}

func (bridge *Bridge) UpdateBotProfile() {
	bridge.Log.Debugln("Updating bot profile")
	botConfig := bridge.Config.AppService.Bot

	var err error
	var mxc id.ContentURI
	if botConfig.Avatar == "remove" {
		err = bridge.Bot.SetAvatarURL(mxc)
	} else if len(botConfig.Avatar) > 0 {
		mxc, err = id.ParseContentURI(botConfig.Avatar)
		if err == nil {
			err = bridge.Bot.SetAvatarURL(mxc)
		}
	}
	if err != nil {
		bridge.Log.Warnln("Failed to update bot avatar:", err)
	}

	if botConfig.Displayname == "remove" {
		err = bridge.Bot.SetDisplayName("")
	} else if len(botConfig.Avatar) > 0 {
		err = bridge.Bot.SetDisplayName(botConfig.Displayname)
	}
	if err != nil {
		bridge.Log.Warnln("Failed to update bot displayname:", err)
	}
}

func (bridge *Bridge) ipcStop(_ json.RawMessage) interface{} {
	bridge.Stop()
	return nil
}

func (bridge *Bridge) Stop() {
	select {
	case bridge.stop <- struct{}{}:
	default:
	}
}

func (bridge *Bridge) internalStop() {
	bridge.stopping = true
	if bridge.Crypto != nil {
		bridge.Crypto.Stop()
	}
	bridge.AS.StopWebsocket(appservice.WebsocketManualStop)
	bridge.EventProcessor.Stop()
	bridge.IMHandler.Stop()
}

func (bridge *Bridge) Main() {
	if *generateRegistration {
		bridge.GenerateRegistration()
		return
	}

	bridge.Init()
	bridge.Log.Infoln("Bridge initialization complete, starting...")
	bridge.Start()
	bridge.Log.Infoln("Bridge started!")

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	select {
	case <-c:
		bridge.Log.Infoln("Interrupt received, stopping...")
	case <-bridge.stop:
		bridge.Log.Infoln("Stop command received, stopping...")
	}

	bridge.internalStop()
	bridge.Log.Infoln("Bridge stopped.")
	os.Exit(0)
}

func main() {
	flag.SetHelpTitles(
		"mautrix-imessage - A Matrix-iMessage puppeting bridge.",
		"mautrix-imessage [-h] [-c <path>] [-r <path>] [-u <url>] [-o] [-g]")
	err := flag.Parse()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		flag.PrintHelp()
		os.Exit(1)
	} else if *wantHelp {
		flag.PrintHelp()
		os.Exit(0)
	} else if *version {
		fmt.Println(VersionString)
		return
	}

	if len(*configURL) > 0 {
		err = config.Download(*configURL, *configPath, *configOutputRedirect)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Failed to download config: %v\n", err)
			os.Exit(2)
		}
	}

	NewBridge().Main()
}
