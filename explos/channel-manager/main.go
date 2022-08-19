package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"runtime"
	"sync"
	"syscall"

	"berty.tech/berty/v2/go/pkg/bertybot"
	"berty.tech/berty/v2/go/pkg/bertyversion"
	"berty.tech/berty/v2/go/pkg/messengertypes"

	qrterminal "github.com/mdp/qrterminal/v3"
	"github.com/oklog/run"
	"github.com/peterbourgon/ff/v3"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"moul.io/climan"
	"moul.io/motd"
	"moul.io/srand"
	"moul.io/zapconfig"
)

func main() {
	if err := mainRun(os.Args[1:]); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "error: %v+\n", err)
		}
		os.Exit(1)
	}
}

var (
	opts struct { // nolint:maligned
		Debug          bool
		BertyNodeAddr  string
		apiAdr         string
		rootLogger     *zap.Logger
		privatekeyPath string
		publickeyPath  string
		generateKeys   bool
	}

	PrivateKey []byte
	PublicKey  []byte
)

func mainRun(args []string) error {
	// parse CLI
	name := os.Args[0]
	root := &climan.Command{
		Name:       name,
		ShortUsage: name + " [global flags] <subcommand> [flags] [args]",
		ShortHelp:  "More info on https://github.com/pmg-tools/berty-bot-experiments.",
		FlagSetBuilder: func(fs *flag.FlagSet) {
			// opts.BertyNodeAddr = ""
			fs.BoolVar(&opts.Debug, "debug", false, "debug mode")
			fs.StringVar(&opts.BertyNodeAddr, "berty-node-addr", "127.0.0.1:9091", "Berty node address")
			fs.StringVar(&opts.apiAdr, "api-adr", "http://127.0.0.1:8080/access", "teritori API address")
			fs.StringVar(&opts.publickeyPath, "publickeyPath", "", "public key")
			fs.StringVar(&opts.privatekeyPath, "privatekeyPath", "", "private key")
			fs.BoolVar(&opts.generateKeys, "generate-keys", false, "generate keys")
		},
		Exec:      doRoot,
		FFOptions: []ff.Option{ff.WithEnvVarPrefix(name)},
	}
	if err := root.Parse(args); err != nil {
		return fmt.Errorf("parse error: %w", err)
	}

	// init runtime
	{
		// prng
		rand.Seed(srand.Fast())

		// concurrency
		runtime.GOMAXPROCS(1)

		// logger
		config := zapconfig.New().SetPreset("light-console")
		if opts.Debug {
			config = config.SetLevel(zapcore.DebugLevel)
		} else {
			config = config.SetLevel(zapcore.InfoLevel)
		}
		var err error
		opts.rootLogger, err = config.Build()
		if err != nil {
			return fmt.Errorf("logger init: %w", err)
		}
	}

	// run
	if err := root.Run(context.Background()); err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

func doRoot(ctx context.Context, args []string) error { // nolint:gocognit
	logger := opts.rootLogger.Named("app")
	logger.Debug("init", zap.Strings("args", args), zap.Any("opts", opts))

	if len(args) > 0 {
		return flag.ErrHelp
	}

	if opts.BertyNodeAddr == "" {
		// FIXME: implement inmem bot.
		return fmt.Errorf("missing --berty-node-addr: %w", flag.ErrHelp)
	}

	fmt.Print(motd.Default())

	var g run.Group
	ctx, cancel := context.WithCancel(ctx)
	g.Add(func() error {
		<-ctx.Done()
		return nil
	}, func(err error) {
		logger.Info("Exiting...", zap.Error(err))
		cancel()
	})
	// signal handling
	g.Add(run.SignalHandler(ctx, syscall.SIGTERM, syscall.SIGINT, os.Interrupt, os.Kill))

	// berty bot
	g.Add(func() error {
		// var dbA = &mockDb{}
		dbA, err := NewSqliteDB()
		if err != nil {
			return fmt.Errorf("db init: %w", err)
		}

		// key err handling
		if opts.generateKeys {
			err = GenKeys("private.key", "public.key")
			if err != nil {
				return err
			}
			opts.privatekeyPath = "private.key"
			opts.publickeyPath = "public.key"
		}

		if opts.privatekeyPath == "" || opts.publickeyPath == "" {
			return fmt.Errorf("missing --privatekeyPath or --publickeyPath: %w", flag.ErrHelp)
		}

		PrivateKey, err = ioutil.ReadFile(opts.privatekeyPath)
		if err != nil {
			return fmt.Errorf("read private key: %w", err)
		}

		PublicKey, err = ioutil.ReadFile(opts.publickeyPath)
		if err != nil {
			return fmt.Errorf("read public key: %w", err)
		}
		//

		mutex := &sync.Mutex{}

		versionCommand := func(ctx bertybot.Context) {
			_ = ctx.ReplyString("version: " + bertyversion.Version)
		}

		cc, err := grpc.Dial(opts.BertyNodeAddr, grpc.WithInsecure())
		if err != nil {
			return fmt.Errorf("dial error: %w", err)
		}
		client := messengertypes.NewMessengerServiceClient(cc)

		botName := os.Args[0]
		newOpts := []bertybot.NewOption{}
		newOpts = append(newOpts,
			bertybot.WithLogger(logger.Named("berty")), // configure a logger
			bertybot.WithDisplayName(botName),          // bot name
			// bertybot.WithHandler(bertybot.UserMessageHandler, userMessageHandler), // message handler
			bertybot.WithCommand("version", "show version", versionCommand),
			bertybot.WithRecipe(bertybot.AutoAcceptIncomingContactRequestRecipe()),
			bertybot.WithRecipe(bertybot.AutoAcceptIncomingGroupInviteRecipe()),
			bertybot.WithRecipe(bertybot.WelcomeMessageRecipe("Hello dear peroquet !")),
			bertybot.WithCommand("ping", "ping", func(ctx bertybot.Context) {
				if ctx.IsReplay || !ctx.IsNew {
					return
				}
				_ = ctx.ReplyString("pong")
			}),

			// CHAN COMMANDS
			bertybot.WithCommand("add-work", "create a channel", bertyBotAddWorkspace(dbA, mutex)),
			bertybot.WithCommand("add-channel", "add a channel", bertyBotAddChannel(dbA, mutex)),
			bertybot.WithCommand("list-workspaces", "list workspaces", bertyBotListWorkspaces(dbA)),
			bertybot.WithCommand("list-channels", "list channels", bertyBotListChannels(dbA)),
			bertybot.WithCommand("refresh-all", "refresh channels", bertyBotRefreshAll(dbA, opts.apiAdr)),
			bertybot.WithCommand("refresh", "refresh", bertyBotRefresh(dbA, opts.apiAdr)),
			//

			// AUTH COMMANDS
			bertybot.WithCommand("link-teritori-account", "auth", TeritoriAuth(dbA)),
			//

			bertybot.WithMessengerClient(client),
		)
		if opts.Debug {
			newOpts = append(newOpts, bertybot.WithRecipe(bertybot.DebugEventRecipe(logger.Named("debug"))))
		}

		bot, err := bertybot.New(newOpts...)
		if err != nil {
			return fmt.Errorf("bot initialization failed: %w", err)
		}
		logger.Info("retrieve instance Berty ID",
			zap.String("pk", bot.PublicKey()),
			zap.String("link", bot.BertyIDURL()),
		)
		if opts.Debug {
			qrterminal.GenerateHalfBlock(bot.BertyIDURL(), qrterminal.L, os.Stdout)
		}

		return bot.Start(ctx)
	}, func(error) {})

	logger.Info("Starting...")
	return g.Run()
}
