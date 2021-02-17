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
	_ "go.mau.fi/mautrix-imessage/imessage/mac"
)

var (
	// These are static
	Name = "mautrix-imessage"
	URL  = "https://github.com/tulir/mautrix-imessage"
	// This is changed when making a release
	Version = "0.1.5"
	// These are filled at build time with the -X linker flag
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func init() {
	if len(Tag) > 0 && Tag[0] == 'v' {
		Tag = Tag[1:]
	}
	if Tag != Version && !strings.HasSuffix(Version, "+dev") {
		Version += "+dev"
	}
}

var configPath = flag.MakeFull("c", "config", "The path to your config file.", "config.yaml").String()

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
	IPC            *IPCHandler

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

	bridge.Log.Debugln("Initializing iMessage connector")
	bridge.IM, err = imessage.NewAPI(bridge.Config.IMessage)
	if err != nil {
		bridge.Log.Fatalln("Failed to initialize iMessage connector:", err)
		os.Exit(14)
	}

	bridge.IMHandler = NewiMessageHandler(bridge)
	bridge.Crypto = NewCryptoHelper(bridge)
	bridge.IPC = NewIPCHandler(bridge)
}

func (bridge *Bridge) startWebsocket() {
	for {
		err := bridge.AS.StartWebsocket(bridge.Config.Homeserver.WSProxy)
		if err != nil {
			bridge.Log.Errorln("Error in appservice websocket:", err)
		}
		if bridge.stopping {
			return
		}
		bridge.Log.Infoln("Websocket disconnected, reconnecting in 5 seconds...")
		time.Sleep(5 * time.Second)
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
	bridge.Log.Debugln("Stating iMessage handler")
	go bridge.IMHandler.Start()
	go bridge.IPC.Loop()
	go bridge.UpdateBotProfile()
	if bridge.Crypto != nil {
		go bridge.Crypto.Start()
	}

	for _, portal := range bridge.GetAllPortals() {
		if len(portal.MXID) > 0 {
			portal.Sync()
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

func (bridge *Bridge) Stop() {
	bridge.stopping = true
	if bridge.Crypto != nil {
		bridge.Crypto.Stop()
	}
	bridge.AS.StopWebsocket()
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

	bridge.Stop()
	bridge.Log.Infoln("Bridge stopped.")
	os.Exit(0)
}

func main() {
	flag.SetHelpTitles(
		"mautrix-imessage - A Matrix-iMessage puppeting bridge.",
		"mautrix-imessage [-h] [-c <path>] [-r <path>] [-g]")
	err := flag.Parse()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		flag.PrintHelp()
		os.Exit(1)
	} else if *wantHelp {
		flag.PrintHelp()
		os.Exit(0)
	} else if *version {
		if Tag == Version {
			fmt.Printf("%s %s (%s)\n", Name, Tag, BuildTime)
		} else if len(Commit) > 8 {
			fmt.Printf("%s %s.%s (%s)\n", Name, Version, Commit[:8], BuildTime)
		} else {
			fmt.Printf("%s %s.unknown\n", Name, Version)
		}
		return
	}

	NewBridge().Main()
}
