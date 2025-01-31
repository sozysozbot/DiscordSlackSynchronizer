package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/kmc-jp/DiscordSlackSynchronizer/configurator"
	"github.com/kmc-jp/DiscordSlackSynchronizer/discord_webhook"
	"github.com/kmc-jp/DiscordSlackSynchronizer/slack_emoji_imager"
	"github.com/kmc-jp/DiscordSlackSynchronizer/slack_webhook"
)

type Token struct {
	Slack struct {
		API   string
		Event string
		User  string
	}
	Discord struct {
		API string
	}
	Gyazo struct {
		API string
	}
}

var Tokens Token
var SettingsFile string

const ProgramName = "DiscordSlackSync"

func init() {
	Tokens.Slack.API = os.Getenv("SLACK_API_TOKEN")
	Tokens.Slack.Event = os.Getenv("SLACK_EVENT_TOKEN")
	Tokens.Discord.API = os.Getenv("DISCORD_BOT_TOKEN")
	Tokens.Slack.User = os.Getenv("SLACK_API_USER_TOKEN")
	SettingsFile = filepath.Join(os.Getenv("STATE_DIRECTORY"), "settings.json")
	if SettingsFile == "" {
		SettingsFile = "settings.json"
	}
}

func main() {
	var settings = NewSettingsHandler(Tokens.Slack.API, Tokens.Discord.API)

	imager, err := slack_emoji_imager.New(Tokens.Slack.User, Tokens.Slack.API)
	if err != nil {
		fmt.Println("Imager initialize error:", err)
	}

	if Tokens.Discord.API == "" {
		fmt.Println("No discord token provided")
		return
	}

	var discordWebhookHandler = discord_webhook.New(Tokens.Discord.API)
	var slackWebhookHandler = slack_webhook.New(Tokens.Slack.API)

	var slackReactionHandler = NewSlackReactionHandler(slackWebhookHandler, discordWebhookHandler, settings)
	slackReactionHandler.SetReactionImager(imager)

	var discordReacionHandler = NewDiscordReactionHandler(slackWebhookHandler, discordWebhookHandler, settings)

	var Discord = NewDiscordBot(Tokens.Discord.API, settings)
	Discord.SetSlackWebhook(slackWebhookHandler)
	Discord.SetDiscordWebhook(discordWebhookHandler)
	Discord.SetDiscordReactionHandler(discordReacionHandler)

	var Slack = NewSlackBot(Tokens.Slack.API, Tokens.Slack.Event, settings)

	Slack.SetUserToken(Tokens.Slack.User)
	Slack.SetDiscordWebhook(discordWebhookHandler)
	Slack.SetSlackWebhook(slackWebhookHandler)
	Slack.SetReactionHandler(slackReactionHandler)

	go func() {
		// start Discord session
		err := Discord.Do()
		if err != nil {
			fmt.Println("Error opening Discord session: ", err)
		}

		fmt.Println("Discord session is now running.  Press CTRL-C to exit.")
	}()
	// start Slack session
	go Slack.Do()

	slackReactionHandler.SetMessageEscaper(Slack)

	var sockType = os.Getenv("SOCK_TYPE")
	var listenAddr = os.Getenv("LISTEN_ADDRESS")

	// start web configurator
	var conf = configurator.New(Tokens.Discord.API, Tokens.Slack.API, SettingsFile)
	switch sockType {
	case "tcp", "unix":
		controller, err := conf.Start(os.Getenv("HTTP_PATH_PREFIX"), sockType, listenAddr)
		if err != nil {
			panic(err)
		}

		go func() {
			for command := range controller {
				switch command {
				case configurator.CommandRestart:
					discordWebhookHandler.Reset()
				default:
					continue
				}
			}
		}()

		fmt.Printf("Start Configuration Server on: %s:%s\n", sockType, listenAddr)
	}

	// wait syscall
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	Discord.Close()
	conf.Close()
}
