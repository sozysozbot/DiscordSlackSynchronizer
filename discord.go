package main

import (
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
	dp "github.com/kmc-jp/DiscordSlackSynchronizer/discord_plugin"
	"github.com/kmc-jp/DiscordSlackSynchronizer/discord_webhook"
	"github.com/kmc-jp/DiscordSlackSynchronizer/slack_webhook"
	"github.com/pkg/errors"
)

const DiscordAPIEndpoint = "https://discord.com/api"
const SlackMessageDummyURI = "http://example?discord_message_ts="

type DiscordHandler struct {
	Session *discordgo.Session
	regExp  struct {
		UserID   *regexp.Regexp
		Channel  *regexp.Regexp
		ImageURI *regexp.Regexp

		replace *regexp.Regexp
		refURI  *regexp.Regexp
	}

	hook *discord_webhook.Handler

	slackLastMessages SlackLastMessages
	slackHook         *slack_webhook.Handler

	reactionHandler *DiscordReactionHandler

	settings *SettingsHandler
}

func NewDiscordBot(apiToken string, settings *SettingsHandler) *DiscordHandler {
	// Create a new Discord session using the provided bot token.
	dg, err := discordgo.New("Bot " + Tokens.Discord.API)
	if err != nil {
		fmt.Println("Error creating Discord session: ", err)
		return nil
	}

	var d DiscordHandler

	d.Session = dg
	d.regExp.UserID = regexp.MustCompile(`<@!(\d+)>`)
	d.regExp.Channel = regexp.MustCompile(`<#(\d+)>`)
	d.regExp.ImageURI = regexp.MustCompile(`\S\.png|\.jpg|\.jpeg|\.gif`)
	d.regExp.replace = regexp.MustCompile(`\s*ss\/(.+)\/(.*)(\/)??\s*`)
	d.regExp.refURI = regexp.MustCompile(`\(RefURI:\s<https:.+>\)`)

	dg.AddHandler(d.voiceState)
	dg.AddHandler(d.watch)
	dg.AddHandler(d.ReactionAdd)
	dg.AddHandler(d.ReactionRemove)
	dg.AddHandler(d.ReactionRemoveAll)

	d.slackLastMessages = SlackLastMessages{}
	d.settings = settings

	return &d
}

func (d *DiscordHandler) SetSlackWebhook(hook *slack_webhook.Handler) {
	d.slackHook = hook
}

func (d *DiscordHandler) SetDiscordWebhook(hook *discord_webhook.Handler) {
	d.hook = hook
}

func (d *DiscordHandler) SetDiscordReactionHandler(handler *DiscordReactionHandler) {
	d.reactionHandler = handler
}

func (d *DiscordHandler) Close() error {
	return d.Session.Close()
}

func (d *DiscordHandler) Do() error {
	return d.Session.Open()
}

func (d *DiscordHandler) watch(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore all messages created by the bot itself
	// This isn't required in this specific example but it's a good practice.
	if m.Author.ID == s.State.User.ID || m.Author.Bot {
		return
	}

	var sdt = d.settings.FindSlackChannel(m.ChannelID, m.GuildID)
	if sdt.SlackChannel == "" {
		return
	}

	//Confirm Discord to Slack
	if !sdt.Setting.DiscordToSlack {
		return
	}

	var reference *discordgo.Message
	var err error

	if m.Message != nil && m.Message.MessageReference != nil {
		reference, err = s.ChannelMessage(m.Message.MessageReference.ChannelID, m.Message.MessageReference.MessageID)
		if err != nil {
			log.Println(err)
			return
		}

		err = func() (err error) {
			if !d.regExp.replace.MatchString(m.Content) {
				return
			}

			id, err := d.parseUserName(reference.Author)
			if err != nil {
				return err
			}

			ids, err := dp.GetDiscordID(id)
			if err != nil {
				return err
			}

			var check bool
			for _, id := range ids {
				if id == m.Author.ID {
					check = true
				}
			}
			if !check {
				return fmt.Errorf("InvalidUpdateMessage")
			}

			err = d.deleteMessage(m.ChannelID, m.ID)
			if err != nil {
				log.Println(err)
			}

			var message discord_webhook.Message
			message.Message = reference
			message.Attachments = make([]discord_webhook.Attachment, 0)

			for i := range message.Message.Attachments {
				if message.Message.Attachments[i] == nil {
					continue
				}

				var oldAtt = message.Message.Attachments[i]

				message.Attachments = append(message.Attachments, discord_webhook.Attachment{
					URL:      oldAtt.URL,
					ID:       oldAtt.ID,
					ProxyURL: oldAtt.ProxyURL,
					Filename: oldAtt.Filename,
					Width:    oldAtt.Width,
					Height:   oldAtt.Height,
					Size:     oldAtt.Size,
				})
			}

			var newContent = reference.Content
			var newContentSlice = strings.Split(newContent, "\n")

			var refMatch = d.regExp.refURI.MatchString(newContentSlice[len(newContentSlice)-1])
			if refMatch {
				newContent = strings.Join(newContentSlice[1:len(newContentSlice)-1], "\n")
			}

			// replace and update message
			for _, pattern := range strings.Split(m.Content, "\n") {
				var matches = strings.Split(pattern, "/")
				var escapedMatch = []string{}
				var tmp string
				for i, match := range matches {
					if i < 1 {
						continue
					}
					if tmp != "" {
						match = tmp + "/" + match
						tmp = ""
					}
					if strings.HasSuffix(match, "\\") && !strings.HasSuffix(match, "\\\\") {
						tmp = strings.TrimSuffix(match, "\\")
						continue
					}

					escapedMatch = append(escapedMatch, match)
				}
				if len(escapedMatch) < 2 {
					return fmt.Errorf("Mal-formedExpression")
				}
				newContent = strings.ReplaceAll(newContent, escapedMatch[0], escapedMatch[1])
			}

			if refMatch {
				newContent = strings.Join(
					[]string{
						newContentSlice[0],
						newContent,
						newContentSlice[len(newContentSlice)-1],
					}, "\n",
				)
			}

			message.Content = newContent
			_, err = d.hook.Edit(message.ChannelID, message.ID, message, []discord_webhook.File{})
			if err != nil {
				log.Printf("EditError: %s\n", err)
				return
			}

			return
		}()
		if err == nil {
			return
		}
	}

	var name string = m.Member.Nick
	if name == "" {
		name = m.Author.Username
	}

	primaryID, err := dp.GetPrimaryID(m.Author.ID)
	if err != nil {
		log.Println(err)
		primaryID = m.Author.ID
	}

	var dMessage = discord_webhook.Message{
		AvaterURL: m.Author.AvatarURL(""),
		UserName:  fmt.Sprintf("%s(%s)", name, primaryID),
		Message: &discordgo.Message{
			ChannelID: m.ChannelID,
			Content:   m.Content,
		},
	}

	var dFiles = []discord_webhook.File{}

	if m.Message != nil {
		if m.Message.Attachments != nil {
			// Add original attachments to new message
			for i := range m.Message.Attachments {
				if m.Message.Attachments[i] == nil {
					continue
				}

				var dfile discord_webhook.File

				resp, err := http.Get(m.Message.Attachments[i].URL)
				if err != nil {
					log.Printf("DownloadErr: %s\n", err.Error())
					continue
				}
				defer resp.Body.Close()

				dfile.Reader = resp.Body
				dfile.FileName = m.Message.Attachments[i].Filename
				dFiles = append(dFiles, dfile)
			}
		}

		if m.Embeds != nil {
			dMessage.Embeds = m.Embeds
		}
	}

	if reference != nil {
		// if sent text has reference, add its first line text to message
		var refText string
		var refSlice = strings.Split(reference.Content, "\n")
		if len(refSlice) > 1 {
			refText = refSlice[0] + "..."
		} else {
			refText = refSlice[0]
		}
		dMessage.Content = fmt.Sprintf("> %s\n%s\n(RefURI: <%s>)",
			refText,
			m.Content,
			fmt.Sprintf("https://discord.com/channels/%s/%s/%s",
				m.GuildID, reference.ChannelID, reference.ID,
			),
		)
	}

	// Delete message on Discord
	err = d.deleteMessage(m.ChannelID, m.ID)
	if err != nil {
		log.Println(err)
	} else {
		// if it was successed, send message by webhook
		message, err := d.hook.Send(m.ChannelID, dMessage, false, dFiles)
		if err != nil {
			log.Printf("MessageSendError: %s", err)
		}

		dMessage = *message
	}

	var imageURIs = []string{}
	var imageTitles = []string{}

	var fileBlocks = []slack_webhook.BlockBase{}

	for _, attach := range dMessage.Attachments {
		if d.regExp.ImageURI.MatchString(attach.URL) {
			// image like png, gif, jpeg
			imageURIs = append(imageURIs, attach.URL)
			imageTitles = append(imageTitles, attach.Filename)
		} else {
			if attach.URL != "" {
				// add rich file link to the slack message
				var externalID = fmt.Sprintf("%s:%s/%s", ProgramName, m.ChannelID, attach.ID)

				_, err := d.slackHook.FilesRemoteAdd(
					slack_webhook.FilesRemoteAddParameters{
						FileType:    slack_webhook.FindFileType(attach.Filename),
						ExternalURL: attach.URL,
						Title:       attach.Filename,
						ExternalID:  externalID,
					},
				)
				if err != nil {
					log.Printf("SlackFileRemoteAddAPIError: %s", err.Error())
					continue
				}

				fileBlocks = append(fileBlocks, slack_webhook.FileBlock(externalID))
			}
		}
	}

	var content = m.Content

	for _, id := range d.regExp.UserID.FindAllStringSubmatch(content, -1) {
		if len(id) < 2 {
			continue
		}

		mem, err := s.GuildMember(m.GuildID, id[1])
		if err != nil {
			continue
		}

		var idName = mem.Nick
		if idName == "" {
			idName = mem.User.Username
		}
		content = strings.Join(strings.Split(content, "!"+id[1]), idName)
	}

	for _, ch := range d.regExp.Channel.FindAllStringSubmatch(content, -1) {
		if len(ch) < 2 {
			continue
		}

		channel, err := s.State.GuildChannel(m.GuildID, ch[1])
		if err != nil {
			continue
		}

		content = strings.Join(strings.Split(content,
			fmt.Sprintf("<#%s>", ch[1])),
			fmt.Sprintf(
				"<https://discord.com/channels/%s/%s|#%s>",
				m.GuildID, ch[1], channel.Name,
			),
		)
	}

	if sdt.Setting.ShowChannelName {
		channelData, err := s.State.GuildChannel(m.GuildID, m.ChannelID)
		if err != nil {
			fmt.Printf("%s\n", err.Error())
			return
		}
		content = "`#" + channelData.Name + "` " + content
	}

	var blocks = []slack_webhook.BlockBase{}
	for i, imageURI := range imageURIs {
		var title = imageTitles[i]
		var block = slack_webhook.ImageBlock(imageURI, title)
		block.Title = slack_webhook.ImageTitle(title, false)
		blocks = append(blocks, block)
	}

	blocks = append(blocks, fileBlocks...)

	// TODO: create channel if not exist option

	// append discord message id
	if m.Message != nil {
		content += fmt.Sprintf(" <%s%s|%s>", SlackMessageDummyURI, m.Message.Timestamp, "ㅤ")
	}

	if len(blocks) > 0 && m.Content != "" {
		var textBlock = slack_webhook.ContextBlock(slack_webhook.MrkdwnElement(content))
		blocks = append([]slack_webhook.BlockBase{textBlock}, blocks...)
	}

	var message = slack_webhook.Message{
		IconURL:     m.Author.AvatarURL(""),
		Username:    name,
		Channel:     sdt.SlackChannel,
		Text:        content,
		Blocks:      blocks,
		UnfurlLinks: true,
		UnfurlMedia: true,
		LinkNames:   true,
	}

	// Send message to Slack
	_, err = d.slackHook.Send(message)
	if err != nil {
		log.Printf("ErrorInSendingMessageToSlack: %s\n", err.Error())
		return
	}

}

type VoiceEvent int

const (
	VoiceLeft         VoiceEvent = iota
	VoiceEmptied      VoiceEvent = iota
	VoiceStateChanged VoiceEvent = iota
	VoiceEntered      VoiceEvent = iota
)

func (d *DiscordHandler) voiceState(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
	voiceChannels.Mutex.Lock()
	defer voiceChannels.Mutex.Unlock()

	if vs.UserID == s.State.User.ID {
		return
	}

	channel, e := s.State.Channel(vs.VoiceState.ChannelID)

	if voiceChannels.Guilds[vs.GuildID] == nil {
		voiceChannels.Guilds[vs.GuildID] = &VoiceChannels{}
	}

	channels := voiceChannels.Guilds[vs.GuildID]

	if e != nil || vs.ChannelID == "" { // If the channel is missing, the user has left
		channel, ok := channels.FindChannelHasUser(vs.UserID)
		if !ok {
			return
		}
		channels.Leave(vs.UserID)
		setting := d.settings.FindSlackChannel(channel, vs.VoiceState.GuildID)
		if len(channels.Channels[channel].Users) == 0 {
			d.sendVoiceState(setting, channels, VoiceEmptied)
		} else {
			d.sendVoiceState(setting, channels, VoiceLeft)
		}
	} else { // User joind or State changed
		setting := d.settings.FindSlackChannel(vs.VoiceState.ChannelID, vs.VoiceState.GuildID)
		mem, err := s.GuildMember(vs.GuildID, vs.UserID)
		if err != nil {
			fmt.Printf("Failed to get info of a member: %v\n", err)
			return
		}
		exists := channels.Join(channel, mem)

		if vs.SelfDeaf {
			channels.Deafened(vs.UserID)
		} else if vs.Mute || vs.SelfMute {
			channels.Muted(vs.UserID)
		}

		if !exists {
			d.sendVoiceState(setting, channels, VoiceEntered)
		} else if setting.Setting.SendMuteState {
			d.sendVoiceState(setting, channels, VoiceStateChanged)
		}
	}
}

func (d *DiscordHandler) ReactionAdd(_ *discordgo.Session, ev *discordgo.MessageReactionAdd) {
	err := d.reactionHandler.GetReaction(ev.GuildID, ev.ChannelID, ev.MessageID)
	if err != nil {
		log.Println(err)
	}
}
func (d *DiscordHandler) ReactionRemove(_ *discordgo.Session, ev *discordgo.MessageReactionRemove) {
	err := d.reactionHandler.GetReaction(ev.GuildID, ev.ChannelID, ev.MessageID)
	if err != nil {
		log.Println(err)
	}
}
func (d *DiscordHandler) ReactionRemoveAll(_ *discordgo.Session, ev *discordgo.MessageReactionRemoveAll) {
	err := d.reactionHandler.GetReaction(ev.GuildID, ev.ChannelID, ev.MessageID)
	if err != nil {
		log.Println(err)
	}
}

func (d *DiscordHandler) sendVoiceState(setting ChannelSetting, channels *VoiceChannels, event VoiceEvent) {
	if setting.SlackChannel == "" {
		return
	}
	if !setting.Setting.SendVoiceState {
		return
	}
	var blocks []slack_webhook.BlockBase
	var err error
	if setting.DiscordChannel == "all" {
		blocks, err = channels.SlackBlocksMultiChannel()
		if err != nil {
			fmt.Printf("%v\n", errors.Wrapf(err, "Failed SlackBlocks"))
			return
		}
	} else {
		channel, ok := channels.Channels[setting.DiscordChannel]
		if !ok {
			fmt.Print("Failed to find channel")
		}
		blocks = channel.SlackBlocksSingleChannel()
		if err != nil {
			fmt.Printf("%v\n", errors.Wrapf(err, "Failed SlackBlock"))
		}
	}

	var message = slack_webhook.Message{
		Channel:     setting.SlackChannel,
		Username:    "Discord Watcher",
		IconEmoji:   "discord",
		UnfurlLinks: false,
		UnfurlMedia: false,
		Blocks:      blocks,
	}

	switch event {
	case VoiceEntered:
		ts, ok := d.slackLastMessages[message.Channel]
		if ok {
			d.slackHook.Remove(message.Channel, ts)
		}

		ts, err = d.slackHook.Send(message)
		if err != nil {
			log.Println(err)
			return
		}

		d.slackLastMessages[message.Channel] = ts
	case VoiceLeft:
		ts, ok := d.slackLastMessages[message.Channel]
		if !ok {
			ts, ok := d.slackLastMessages[message.Channel]
			if ok {
				delete(d.slackLastMessages, message.Channel)
				d.slackHook.Remove(message.Channel, ts)
			}

			ts, err = d.slackHook.Send(message)
			if err != nil {
				log.Println(err)
				return
			}
			d.slackLastMessages[message.Channel] = ts
		}
		message.TS = ts
		ts, err = d.slackHook.Update(message)
		if err != nil {
			log.Println(err)
			return
		}

		d.slackLastMessages[message.Channel] = ts

	case VoiceStateChanged:
		ts, ok := d.slackLastMessages[message.Channel]
		if !ok {
			d.slackHook.Send(message)
		}

		message.TS = ts
		ts, err = d.slackHook.Update(message)
		if err != nil {
			log.Println(err)
			return
		}

		d.slackLastMessages[message.Channel] = ts

	case VoiceEmptied:
		ts, ok := d.slackLastMessages[message.Channel]
		if !ok {
			return
		}
		delete(d.slackLastMessages, message.Channel)

		d.slackHook.Remove(message.Channel, ts)
	}
}

func (d *DiscordHandler) deleteMessage(channelID, messageID string) (err error) {
	req, err := http.NewRequest(
		"DELETE",
		fmt.Sprintf("%s/channels/%s/messages/%s",
			DiscordAPIEndpoint, channelID, messageID,
		),
		nil,
	)
	if err != nil {
		return
	}

	req.Header.Set("Authorization", d.Session.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 204 {
		return fmt.Errorf("FailedToDeleteMessage")
	}

	return nil
}

func (d *DiscordHandler) parseUserName(m *discordgo.User) (string, error) {
	var nameSlice = strings.Split(m.Username, "(")
	if len(nameSlice) < 1 {
		return "", fmt.Errorf("Mal-FormedName")
	}

	var id = strings.Split(nameSlice[len(nameSlice)-1], ")")[0]
	return id, nil
}
