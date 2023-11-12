package whatsapp

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"watgbridge/database"
	"watgbridge/state"
	"watgbridge/utils"

	"github.com/PaulSonOfLars/gotgbot/v2"
	goVCard "github.com/emersion/go-vcard"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/zap"
	"golang.org/x/exp/slices"
	"google.golang.org/protobuf/proto"
)

func WhatsAppEventHandler(evt interface{}) {

	cfg := state.State.Config

	switch v := evt.(type) {

	case *events.Receipt:
		ReceiptEventHandler(v)

	case *events.Picture:
		if !cfg.WhatsApp.SkipProfilePictureUpdates {
			PictureEventHandler(v)
		}

	case *events.GroupInfo:
		if !cfg.WhatsApp.SkipGroupSettingsUpdates {
			GroupInfoEventHandler(v)
		}

	case *events.PushName:
		PushNameEventHandler(v)

	case *events.CallOffer:
		CallOfferEventHandler(v)

	case *events.Message:

		isEdited := false
		if protoMsg := v.Message.GetProtocolMessage(); protoMsg != nil &&
			protoMsg.GetType() == waProto.ProtocolMessage_MESSAGE_EDIT {
			isEdited = true
		}

		if v.Info.Timestamp.UTC().Before(state.State.StartTime) {
			// Old events
			return
		}

		if protoMsg := v.Message.GetProtocolMessage(); protoMsg != nil &&
			protoMsg.GetType() == waProto.ProtocolMessage_REVOKE {
			RevokedMessageEventHandler(v)
			return
		}

		text := ""
		if isEdited {
			msg := v.Message.GetProtocolMessage().GetEditedMessage()
			if extendedMessageText := msg.GetExtendedTextMessage().GetText(); extendedMessageText != "" {
				text = extendedMessageText
			} else {
				text = msg.GetConversation()
			}
		} else {
			if extendedMessageText := v.Message.GetExtendedTextMessage().GetText(); extendedMessageText != "" {
				text = extendedMessageText
			} else {
				text = v.Message.GetConversation()
			}
		}

		if v.Info.IsFromMe {
			MessageFromMeEventHandler(text, v, isEdited)
		} else {
			MessageFromOthersEventHandler(text, v, isEdited)
		}
	}

}

func MessageFromMeEventHandler(text string, v *events.Message, isEdited bool) {
	logger := state.State.Logger
	defer logger.Sync()

	var msgId string
	if isEdited {
		msgId = v.Message.GetProtocolMessage().GetKey().GetId()
	} else {
		msgId = v.Info.ID
	}

	// Get ID of the current chat
	if text == ".id" {
		waClient := state.State.WhatsAppClient

		_, err := waClient.SendMessage(context.Background(), v.Info.Chat, &waProto.Message{
			ExtendedTextMessage: &waProto.ExtendedTextMessage{
				Text: proto.String(fmt.Sprintf("The ID of the current chat is:\n```%s```", v.Info.Chat.String())),
				ContextInfo: &waProto.ContextInfo{
					StanzaId:      proto.String(msgId),
					Participant:   proto.String(v.Info.MessageSource.Sender.String()),
					QuotedMessage: v.Message,
				},
			},
		})
		if err != nil {
			logger.Error("Failed to reply to .id command",
				zap.String("event_id", v.Info.ID),
				zap.Error(err),
			)
		}
	}

	if !isEdited {
		// Tag everyone in the group
		textSplit := strings.Fields(strings.ToLower(text))
		if v.Info.IsGroup &&
			(slices.Contains(textSplit, "@all") || slices.Contains(textSplit, "@everyone")) {

			utils.WaTagAll(v.Info.Chat, v.Message, msgId, v.Info.MessageSource.Sender.String(), true)
		}
	}

	if state.State.Config.WhatsApp.SendMyMessagesFromOtherDevices {
		MessageFromOthersEventHandler(text, v, isEdited)
	}
}

func MessageFromOthersEventHandler(text string, v *events.Message, isEdited bool) {
	var (
		cfg      = state.State.Config
		logger   = state.State.Logger
		tgBot    = state.State.TelegramBot
		waClient = state.State.WhatsAppClient
	)
	defer logger.Sync()

	var msgId string
	if isEdited {
		msgId = v.Message.GetProtocolMessage().GetKey().GetId()
	} else {
		msgId = v.Info.ID
	}

	if !isEdited {
		// Return if duplicate event is emitted
		tgChatId, _, _, _ := database.MsgIdGetTgFromWa(msgId, v.Info.Chat.String())
		if tgChatId == cfg.Telegram.TargetChatID {
			logger.Debug("returning because duplicate event id emitted",
				zap.String("event_id", v.Info.ID),
				zap.String("chat_jid", v.Info.Chat.String()),
			)
			return
		}
	}

	if !v.Info.IsFromMe {
		// Return if status is from ignored chat
		if v.Info.Chat.String() == "status@broadcast" &&
			(cfg.WhatsApp.SkipStatus ||
				slices.Contains(cfg.WhatsApp.StatusIgnoredChats, v.Info.MessageSource.Sender.User)) {
			logger.Debug("returning because status from a ignored chat",
				zap.String("event_id", v.Info.ID),
				zap.String("chat_jid", v.Info.Chat.String()),
			)
			return
		} else if slices.Contains(cfg.WhatsApp.IgnoreChats, v.Info.Chat.User) {
			logger.Debug("returning because message from an ignored chat",
				zap.String("event_id", v.Info.ID),
				zap.String("chat_jid", v.Info.Chat.String()),
			)
			return
		}
	}

	replyMarkup := utils.TgBuildUrlButton(utils.WaGetContactName(v.Info.Sender), fmt.Sprintf("https://wa.me/%s", v.Info.MessageSource.Sender.ToNonAD().User))
	if !isEdited {
		if lowercaseText := strings.ToLower(text); !v.Info.IsFromMe && v.Info.IsGroup && slices.Contains(cfg.WhatsApp.TagAllAllowedGroups, v.Info.Chat.User) &&
			(strings.Contains(lowercaseText, "@all") || strings.Contains(lowercaseText, "@everyone")) {
			logger.Debug("usage of @all/@everyone command from your account",
				zap.String("event_id", v.Info.ID),
				zap.String("chat_jid", v.Info.Chat.String()),
			)
			utils.WaTagAll(v.Info.Chat, v.Message, msgId, v.Info.MessageSource.Sender.String(), false)
		}
	}

	var bridgedText string
	if cfg.WhatsApp.SkipChatDetails {
		logger.Debug("skipping to add chat details as configured",
			zap.String("event_id", v.Info.ID),
		)
		if v.Info.IsIncomingBroadcast() {
			bridgedText += "<b>#Broadcast</b>\n"
		} else if v.Info.IsFromMe {
			bridgedText += "<b>You</b>\n"
		} else if v.Info.IsGroup {
			bridgedText += fmt.Sprintf("<b>%s</b>\n", html.EscapeString(utils.WaGetContactName(v.Info.MessageSource.Sender)))
		}

	} else {

		if v.Info.IsFromMe {
			bridgedText += "<b>You</b>\n"
		} else {
			bridgedText += fmt.Sprintf("<b>%s</b>\n", html.EscapeString(utils.WaGetContactName(v.Info.MessageSource.Sender)))
		}
		if v.Info.IsIncomingBroadcast() {
			bridgedText += "<b>#Broadcast</b>\n"
		} else if v.Info.IsGroup {
			bridgedText += fmt.Sprintf("<b>%s</b>\n", html.EscapeString(utils.WaGetGroupName(v.Info.Chat)))
		} else {
			bridgedText += "<b>#Private</b>\n"
		}

	}

	if isEdited {
		bridgedText += "<b>Edited</b>\n"
	}

	if time.Since(v.Info.Timestamp).Seconds() > 60 {
		bridgedText += fmt.Sprintf("<b>%s</b>\n",
			html.EscapeString(v.Info.Timestamp.In(state.State.LocalLocation).Format(cfg.TimeFormat)))
	}

	var (
		replyToMsgId  int64
		threadId      int64
		threadIdFound bool
	)

	if isEdited {

		tgChatId, tgThreadId, tgMsgId, err := database.MsgIdGetTgFromWa(
			v.Message.GetProtocolMessage().GetKey().GetId(),
			v.Info.Chat.String(),
		)
		if err == nil && tgChatId == cfg.Telegram.TargetChatID {
			replyToMsgId = tgMsgId
			threadId = tgThreadId
			threadIdFound = true
		}

	} else {

		logger.Debug("trying to retrieve context info from Message",
			zap.String("event_id", v.Info.ID),
		)
		var contextInfo *waProto.ContextInfo = nil

		if v.Message.GetExtendedTextMessage().GetContextInfo() != nil {
			logger.Debug("taking context info from ExtendedTextMessage",
				zap.String("event_id", v.Info.ID),
			)
			contextInfo = v.Message.GetExtendedTextMessage().GetContextInfo()
		} else if v.Message.GetImageMessage() != nil {
			logger.Debug("taking context info from ImageMessage",
				zap.String("event_id", v.Info.ID),
			)
			contextInfo = v.Message.GetImageMessage().GetContextInfo()
		} else if v.Message.GetVideoMessage() != nil {
			logger.Debug("taking context info from VideoMessage",
				zap.String("event_id", v.Info.ID),
			)
			contextInfo = v.Message.GetVideoMessage().GetContextInfo()
		} else if v.Message.GetAudioMessage() != nil {
			logger.Debug("taking context info from AudioMessage",
				zap.String("event_id", v.Info.ID),
			)
			contextInfo = v.Message.GetAudioMessage().GetContextInfo()
		} else if v.Message.GetDocumentMessage() != nil {
			logger.Debug("taking context info from DocumentMessage",
				zap.String("event_id", v.Info.ID),
			)
			contextInfo = v.Message.GetDocumentMessage().GetContextInfo()
		} else if v.Message.GetStickerMessage() != nil {
			logger.Debug("taking context info from StickerMessage",
				zap.String("event_id", v.Info.ID),
			)
			contextInfo = v.Message.GetStickerMessage().GetContextInfo()
		} else if v.Message.GetContactMessage() != nil {
			logger.Debug("taking context info from ContactMessage",
				zap.String("event_id", v.Info.ID),
			)
			contextInfo = v.Message.GetContactMessage().GetContextInfo()
		} else if v.Message.GetContactsArrayMessage() != nil {
			logger.Debug("taking context info from ContactsArrayMessage",
				zap.String("event_id", v.Info.ID),
			)
			contextInfo = v.Message.GetContactsArrayMessage().GetContextInfo()
		} else if v.Message.GetLocationMessage() != nil {
			logger.Debug("taking context info from LocationMessage",
				zap.String("event_id", v.Info.ID),
			)
			contextInfo = v.Message.GetLocationMessage().GetContextInfo()
		} else if v.Message.GetLiveLocationMessage() != nil {
			logger.Debug("taking context info from LiveLocationMessage",
				zap.String("event_id", v.Info.ID),
			)
			contextInfo = v.Message.GetLiveLocationMessage().GetContextInfo()
		} else if v.Message.GetPollCreationMessage() != nil {
			logger.Debug("taking context info from PollCreationMessage",
				zap.String("event_id", v.Info.ID),
			)
			contextInfo = v.Message.GetPollCreationMessage().GetContextInfo()
		} else if v.Message.GetPollCreationMessageV2() != nil {
			logger.Debug("taking context info from PollCreationMessageV2",
				zap.String("event_id", v.Info.ID),
			)
			contextInfo = v.Message.GetPollCreationMessageV2().GetContextInfo()
		} else if v.Message.GetPollCreationMessageV3() != nil {
			logger.Debug("taking context info from PollCreationMessageV3",
				zap.String("event_id", v.Info.ID),
			)
			contextInfo = v.Message.GetPollCreationMessageV3().GetContextInfo()
		} else {
			logger.Debug("no context info found in any kind of messages",
				zap.String("event_id", v.Info.ID),
			)
		}

		if contextInfo != nil {

			if contextInfo.GetIsForwarded() {
				bridgedText += fmt.Sprintf("<b>Forwarded (%v)</b>\n", contextInfo.GetForwardingScore())
			}

			logger.Debug("checking if your account is mentioned in the message",
				zap.String("event_id", v.Info.ID),
			)
			if mentioned := contextInfo.GetMentionedJid(); v.Info.IsGroup && mentioned != nil {
				for _, jid := range mentioned {
					parsedJid, _ := utils.WaParseJID(jid)
					if parsedJid.User == waClient.Store.ID.User {

						tagInfoText := fmt.Sprintf("<b>%s</b>",
							html.EscapeString(utils.WaGetGroupName(v.Info.Chat)))

						threadId, err := utils.TgGetOrMakeThreadFromWa("#Mentions", cfg.Telegram.TargetChatID, "#Mentions")
						if err != nil {
							utils.TgSendErrorById(tgBot, cfg.Telegram.TargetChatID, 0, "Failed to create/find thread id for 'mentions'", err)
						} else {
							tgBot.SendMessage(cfg.Telegram.TargetChatID, tagInfoText, &gotgbot.SendMessageOpts{
								MessageThreadId: threadId,
								ReplyMarkup:     replyMarkup,
							})
						}

						break
					}
				}
			}

			logger.Debug("trying to retrieve mapped Message in Telegram",
				zap.String("event_id", v.Info.ID),
			)
			stanzaId := contextInfo.GetStanzaId()
			tgChatId, tgThreadId, tgMsgId, err := database.MsgIdGetTgFromWa(stanzaId, v.Info.Chat.String())
			if err == nil && tgChatId == cfg.Telegram.TargetChatID {
				replyToMsgId = tgMsgId
				threadId = tgThreadId
				threadIdFound = true
			}
		}
	}

	if !strings.HasSuffix(bridgedText, "\n\n") {
		bridgedText += "\n"
	}

	if !threadIdFound {
		var err error
		if v.Info.Chat.String() == "status@broadcast" {
			threadId, err = utils.TgGetOrMakeThreadFromWa("status@broadcast", cfg.Telegram.TargetChatID,
				"#Stories")
			if err != nil {
				utils.TgSendErrorById(tgBot, cfg.Telegram.TargetChatID, 0, "Failed to create/find thread id for 'status@broadcast'", err)
				return
			}
		} else if v.Info.IsIncomingBroadcast() {
			threadId, err = utils.TgGetOrMakeThreadFromWa(v.Info.MessageSource.Sender.ToNonAD().String(), cfg.Telegram.TargetChatID,
				utils.WaGetContactName(v.Info.MessageSource.Sender))
			if err != nil {
				utils.TgSendErrorById(tgBot, cfg.Telegram.TargetChatID, 0, fmt.Sprintf("Failed to create/find thread id for <b>%s</b>",
					v.Info.MessageSource.Sender.ToNonAD().String()), err)
				return
			}
		} else if v.Info.IsGroup {
			threadId, err = utils.TgGetOrMakeThreadFromWa(v.Info.Chat.String(), cfg.Telegram.TargetChatID,
				utils.WaGetGroupName(v.Info.Chat))
			if err != nil {
				utils.TgSendErrorById(tgBot, cfg.Telegram.TargetChatID, 0, fmt.Sprintf("Failed to create/find thread id for <b>%s</b>",
					v.Info.Chat.String()), err)
				return
			}
		} else {
			var target_chat_jid waTypes.JID
			if v.Info.IsFromMe {
				target_chat_jid = v.Info.Chat
			} else {
				target_chat_jid = v.Info.Chat
			}

			threadId, err = utils.TgGetOrMakeThreadFromWa(target_chat_jid.ToNonAD().String(), cfg.Telegram.TargetChatID, utils.WaGetContactName(target_chat_jid))
			if err != nil {
				utils.TgSendErrorById(tgBot, cfg.Telegram.TargetChatID, 0, fmt.Sprintf("Failed to create/find thread id for <b>%s</b>",
					target_chat_jid.ToNonAD().String()), err)
				return
			}
		}
	}

	if v.Message.GetImageMessage() != nil {

		imageMsg := v.Message.GetImageMessage()
		if imageMsg.GetUrl() == "" {
			return
		}

		if cfg.WhatsApp.SkipImages {
			bridgedText += "\nSkipping image because 'skip_images' set in config file"
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		} else if !cfg.Telegram.SelfHostedAPI && imageMsg.GetFileLength() > utils.UploadSizeLimit {
			bridgedText += "\nCouldn't send the photo as it exceeds Telegram size restrictions."
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		} else {
			imageBytes, err := waClient.Download(imageMsg)
			if err != nil {
				bridgedText += "\nCouldn't download the photo due to some errors"
				sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
					ReplyToMessageId: replyToMsgId,
					MessageThreadId:  threadId,
				})
				if sentMsg.MessageId != 0 {
					database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
						cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
				}
				return
			}

			if caption := imageMsg.GetCaption(); caption != "" {
				if len(caption) > 1020 {
					bridgedText += html.EscapeString(utils.SubString(caption, 0, 1020)) + "..."
				} else {
					bridgedText += html.EscapeString(caption)
				}
			}

			sentMsg, _ := tgBot.SendPhoto(cfg.Telegram.TargetChatID, imageBytes, &gotgbot.SendPhotoOpts{
				Caption:          bridgedText,
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		}

	} else if v.Message.GetVideoMessage() != nil && v.Message.GetVideoMessage().GetGifPlayback() {

		gifMsg := v.Message.GetVideoMessage()
		if gifMsg.GetUrl() == "" {
			return
		}

		if cfg.WhatsApp.SkipGIFs {
			bridgedText += "\nSkipping GIF because 'skip_gifs' set in config file"
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		} else if !cfg.Telegram.SelfHostedAPI && gifMsg.GetFileLength() > utils.UploadSizeLimit {
			bridgedText += "\nCouldn't send the GIF as it exceeds Telegram size restrictions."
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		} else {
			gifBytes, err := waClient.Download(gifMsg)
			if err != nil {
				bridgedText += "\nCouldn't download the GIF due to some errors"
				sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
					ReplyToMessageId: replyToMsgId,
					MessageThreadId:  threadId,
				})
				if sentMsg.MessageId != 0 {
					database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
						cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
				}
				return
			}

			if caption := gifMsg.GetCaption(); caption != "" {
				if len(caption) > 1020 {
					bridgedText += html.EscapeString(utils.SubString(caption, 0, 1020)) + "..."
				} else {
					bridgedText += html.EscapeString(caption)
				}
			}

			fileToSend := gotgbot.NamedFile{
				FileName: "animation.gif",
				File:     bytes.NewReader(gifBytes),
			}

			sentMsg, _ := tgBot.SendAnimation(cfg.Telegram.TargetChatID, fileToSend, &gotgbot.SendAnimationOpts{
				Caption:          bridgedText,
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		}

	} else if v.Message.GetVideoMessage() != nil {

		videoMsg := v.Message.GetVideoMessage()
		if videoMsg.GetUrl() == "" {
			return
		}

		if cfg.WhatsApp.SkipVideos {
			bridgedText += "\nSkipping video because 'skip_videos' set in config file"
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		} else if !cfg.Telegram.SelfHostedAPI && videoMsg.GetFileLength() > utils.UploadSizeLimit {
			bridgedText += "\nCouldn't send the video as it exceeds Telegram size restrictions."
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		} else {
			videoBytes, err := waClient.Download(videoMsg)
			if err != nil {
				bridgedText += "\nCouldn't download the video due to some errors"
				sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
					ReplyToMessageId: replyToMsgId,
					MessageThreadId:  threadId,
				})
				if sentMsg.MessageId != 0 {
					database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
						cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
				}
				return
			}

			if caption := videoMsg.GetCaption(); caption != "" {
				if len(caption) > 1020 {
					bridgedText += html.EscapeString(utils.SubString(caption, 0, 1020)) + "..."
				} else {
					bridgedText += html.EscapeString(caption)
				}
			}

			fileToSend := gotgbot.NamedFile{
				FileName: "video." + strings.Split(videoMsg.GetMimetype(), "/")[1],
				File:     bytes.NewReader(videoBytes),
			}

			sentMsg, _ := tgBot.SendVideo(cfg.Telegram.TargetChatID, fileToSend, &gotgbot.SendVideoOpts{
				Caption:          bridgedText,
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		}

	} else if v.Message.GetAudioMessage() != nil && v.Message.GetAudioMessage().GetPtt() {

		audioMsg := v.Message.GetAudioMessage()
		if audioMsg.GetUrl() == "" {
			return
		}

		if cfg.WhatsApp.SkipVoiceNotes {
			bridgedText += "\nSkipping voice note because 'skip_voice_notes' set in config file"
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		} else if !cfg.Telegram.SelfHostedAPI && audioMsg.GetFileLength() > utils.UploadSizeLimit {
			bridgedText += "\nCouldn't send the audio as it exceeds Telegram size restrictions."
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		} else {
			audioBytes, err := waClient.Download(audioMsg)
			if err != nil {
				bridgedText += "\nCouldn't download the audio due to some errors"
				sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
					ReplyToMessageId: replyToMsgId,
					MessageThreadId:  threadId,
				})
				if sentMsg.MessageId != 0 {
					database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
						cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
				}
				return
			}

			fileToSend := gotgbot.NamedFile{
				FileName: "audio.ogg",
				File:     bytes.NewReader(audioBytes),
			}

			sentMsg, _ := tgBot.SendAudio(cfg.Telegram.TargetChatID, fileToSend, &gotgbot.SendAudioOpts{
				Caption:          bridgedText,
				Duration:         int64(audioMsg.GetSeconds()),
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		}

	} else if v.Message.GetAudioMessage() != nil {

		audioMsg := v.Message.GetAudioMessage()
		if audioMsg.GetUrl() == "" {
			return
		}

		if cfg.WhatsApp.SkipAudios {
			bridgedText += "\nSkipping audio because 'skip_audios' set in config file"
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		} else if !cfg.Telegram.SelfHostedAPI && audioMsg.GetFileLength() > utils.UploadSizeLimit {
			bridgedText += "\nCouldn't send the audio as it exceeds Telegram size restrictions."
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		} else {
			audioBytes, err := waClient.Download(audioMsg)
			if err != nil {
				bridgedText += "\nCouldn't download the audio due to some errors"
				sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
					ReplyToMessageId: replyToMsgId,
					MessageThreadId:  threadId,
				})
				if sentMsg.MessageId != 0 {
					database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
						cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
				}
				return
			}

			fileToSend := gotgbot.NamedFile{
				FileName: "audio.m4a",
				File:     bytes.NewReader(audioBytes),
			}

			sentMsg, _ := tgBot.SendAudio(cfg.Telegram.TargetChatID, fileToSend, &gotgbot.SendAudioOpts{
				Caption:          bridgedText,
				Duration:         int64(audioMsg.GetSeconds()),
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		}

	} else if v.Message.GetDocumentMessage() != nil {

		documentMsg := v.Message.GetDocumentMessage()
		if documentMsg.GetUrl() == "" {
			return
		}

		if cfg.WhatsApp.SkipDocuments {
			bridgedText += "\nSkipping document because 'skip_documents' set in config file"
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		} else if !cfg.Telegram.SelfHostedAPI && documentMsg.GetFileLength() > utils.UploadSizeLimit {
			bridgedText += "\nCouldn't send the document as it exceeds Telegram size restrictions."
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		} else {
			documentBytes, err := waClient.Download(documentMsg)
			if err != nil {
				bridgedText += "\nCouldn't download the document due to some errors"
				sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
					ReplyToMessageId: replyToMsgId,
					MessageThreadId:  threadId,
				})
				if sentMsg.MessageId != 0 {
					database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
						cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
				}
				return
			}

			if caption := documentMsg.GetCaption(); caption != "" {
				if len(caption) > 1020 {
					bridgedText += html.EscapeString(utils.SubString(caption, 0, 1020)) + "..."
				} else {
					bridgedText += html.EscapeString(caption)
				}
			}

			fileToSend := gotgbot.NamedFile{
				FileName: documentMsg.GetFileName(),
				File:     bytes.NewReader(documentBytes),
			}

			sentMsg, _ := tgBot.SendDocument(cfg.Telegram.TargetChatID, fileToSend, &gotgbot.SendDocumentOpts{
				Caption:          bridgedText,
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		}

	} else if v.Message.GetStickerMessage() != nil {

		stickerMsg := v.Message.GetStickerMessage()
		if stickerMsg.GetUrl() == "" {
			return
		}

		if cfg.WhatsApp.SkipStickers {
			bridgedText += "\nSkipping sticker because 'skip_stickers' set in config file"
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		} else if !cfg.Telegram.SelfHostedAPI && stickerMsg.GetFileLength() > utils.UploadSizeLimit {
			bridgedText += "\nCouldn't send the sticker as it exceeds Telegram size restrictions."
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		} else {
			stickerBytes, err := waClient.Download(stickerMsg)
			if err != nil {
				bridgedText += "\nCouldn't download the sticker due to some errors"
				sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
					ReplyToMessageId: replyToMsgId,
					MessageThreadId:  threadId,
				})
				if sentMsg.MessageId != 0 {
					database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
						cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
				}
				return
			}
			if stickerMsg.GetIsAnimated() || stickerMsg.GetIsAvatar() {
				gifBytes, err := utils.AnimatedWebpConvertToGif(stickerBytes, v.Info.ID)
				if err != nil {
					goto WEBP_TO_GIF_FAILED
				}

				fileToSend := gotgbot.NamedFile{
					FileName: "animation.gif",
					File:     bytes.NewReader(gifBytes),
				}

				sentMsg, _ := tgBot.SendAnimation(cfg.Telegram.TargetChatID, fileToSend, &gotgbot.SendAnimationOpts{
					Caption:          bridgedText,
					ReplyToMessageId: replyToMsgId,
					MessageThreadId:  threadId,
					ReplyMarkup:      replyMarkup,
				})
				if sentMsg.MessageId != 0 {
					database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
						cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
				}
				return

			}
		WEBP_TO_GIF_FAILED:
			sentMsg, _ := tgBot.SendSticker(cfg.Telegram.TargetChatID, stickerBytes, &gotgbot.SendStickerOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
				ReplyMarkup:      replyMarkup,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
		}

	} else if v.Message.GetContactMessage() != nil {
		contactMsg := v.Message.GetContactMessage()

		if cfg.WhatsApp.SkipContacts {
			bridgedText += "\nSkipping contact because 'skip_contacts' set in config file"
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		}

		decoder := goVCard.NewDecoder(bytes.NewReader([]byte(contactMsg.GetVcard())))
		card, err := decoder.Decode()
		if err != nil {
			bridgedText += "\nCouldn't send the vCard as failed to parse it"
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		}

		sentMsg, _ := tgBot.SendContact(cfg.Telegram.TargetChatID, card.PreferredValue(goVCard.FieldTelephone), contactMsg.GetDisplayName(),
			&gotgbot.SendContactOpts{
				Vcard:            contactMsg.GetVcard(),
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
				ReplyMarkup:      replyMarkup,
			})
		if sentMsg.MessageId != 0 {
			database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
				cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
		}
		return

	} else if v.Message.GetContactsArrayMessage() != nil {

		contactsMsg := v.Message.GetContactsArrayMessage()

		if cfg.WhatsApp.SkipContacts {
			bridgedText += "\nSkipping contact array because 'skip_contacts' set in config file"
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		}
		for _, contactMsg := range contactsMsg.Contacts {
			decoder := goVCard.NewDecoder(bytes.NewReader([]byte(contactMsg.GetVcard())))
			card, err := decoder.Decode()
			if err != nil {
				tgBot.SendMessage(cfg.Telegram.TargetChatID, "Couldn't send the vCard as failed to parse it",
					&gotgbot.SendMessageOpts{
						ReplyToMessageId: replyToMsgId,
						MessageThreadId:  threadId,
					})
				continue
			}

			sentMsg, _ := tgBot.SendContact(cfg.Telegram.TargetChatID, card.PreferredValue(goVCard.FieldTelephone), contactMsg.GetDisplayName(),
				&gotgbot.SendContactOpts{
					Vcard:            contactMsg.GetVcard(),
					ReplyToMessageId: replyToMsgId,
					MessageThreadId:  threadId,
					ReplyMarkup:      replyMarkup,
				})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
		}
		return

	} else if v.Message.GetLocationMessage() != nil {

		locationMsg := v.Message.GetLocationMessage()

		if cfg.WhatsApp.SkipLocations {
			bridgedText += "\nSkipping location because 'skip_locations' set in config file"
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		}
		sentMsg, _ := tgBot.SendLocation(cfg.Telegram.TargetChatID, locationMsg.GetDegreesLatitude(), locationMsg.GetDegreesLongitude(),
			&gotgbot.SendLocationOpts{
				HorizontalAccuracy: float64(locationMsg.GetAccuracyInMeters()),
				ReplyToMessageId:   replyToMsgId,
				MessageThreadId:    threadId,
			})
		if sentMsg.MessageId != 0 {
			database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
				cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
		}

		return

	} else if v.Message.GetLiveLocationMessage() != nil {

		bridgedText += "\nShared their live location with you"

		if cfg.WhatsApp.SkipLocations {
			bridgedText += "\nSkipping live location because 'skip_locations' set in config file"
			sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
				ReplyToMessageId: replyToMsgId,
				MessageThreadId:  threadId,
			})
			if sentMsg.MessageId != 0 {
				database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
					cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
			}
			return
		}

		sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
			ReplyToMessageId: replyToMsgId,
			MessageThreadId:  threadId,
		})
		if sentMsg.MessageId != 0 {
			database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
				cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
		}
		return

	} else if v.Message.GetPollCreationMessage() != nil || v.Message.GetPollCreationMessageV2() != nil || v.Message.GetPollCreationMessageV3() != nil {

		var pollMsg *waProto.PollCreationMessage
		if i := v.Message.GetPollCreationMessage(); i != nil {
			pollMsg = i
		} else if i := v.Message.GetPollCreationMessageV2(); i != nil {
			pollMsg = i
		} else if i := v.Message.GetPollCreationMessageV3(); i != nil {
			pollMsg = i
		}

		bridgedText += fmt.Sprintf("%s(<b>%v</b>)\n",
			html.EscapeString(pollMsg.GetName()), pollMsg.GetSelectableOptionsCount())
		for optionNum, option := range pollMsg.GetOptions() {
			if len(bridgedText) > 4000 {
				bridgedText += "\n..."
				break
			}
			bridgedText += fmt.Sprintf("%v. %s\n", optionNum+1, html.EscapeString(option.GetOptionName()))
		}

		sentMsg, _ := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
			ReplyToMessageId: replyToMsgId,
			MessageThreadId:  threadId,
		})
		if sentMsg.MessageId != 0 {
			database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
				cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
		}
		return

	} else {
		if text == "" {
			return
		}

		if len(text) > 4000 {
			bridgedText += html.EscapeString(utils.SubString(text, 0, 4000)) + "..."
		} else {
			bridgedText += html.EscapeString(text)
		}

		if mentioned := v.Message.GetExtendedTextMessage().GetContextInfo().GetMentionedJid(); mentioned != nil {
			for _, jid := range mentioned {
				parsedJid, _ := utils.WaParseJID(jid)
				name := utils.WaGetContactName(parsedJid)
				// text = strings.ReplaceAll(text, "@"+parsedJid.User, "@("+html.EscapeString(name)+")")
				bridgedText = strings.ReplaceAll(
					bridgedText, "@"+parsedJid.User,
					fmt.Sprintf(
						"<a href=\"https://wa.me/%s\">@%s</a>",
						parsedJid.User, html.EscapeString(name),
					),
				)
			}
		}
		sentMsg, err := tgBot.SendMessage(cfg.Telegram.TargetChatID, bridgedText, &gotgbot.SendMessageOpts{
			ReplyToMessageId: replyToMsgId,
			MessageThreadId:  threadId,
		})
		if err != nil {
			panic(fmt.Errorf("Failed to send telegram message: %s", err))
		}
		if sentMsg.MessageId != 0 {
			database.MsgIdAddNewPair(msgId, v.Info.MessageSource.Sender.String(), v.Info.Chat.String(),
				cfg.Telegram.TargetChatID, sentMsg.MessageId, sentMsg.MessageThreadId)
		}
		return
	}
}

func CallOfferEventHandler(v *events.CallOffer) {
	var (
		cfg   = state.State.Config
		tgBot = state.State.TelegramBot
	)

	// TODO : Check and handle group calls
	callerName := utils.WaGetContactName(v.CallCreator)

	callThreadId, err := utils.TgGetOrMakeThreadFromWa("#Calls", cfg.Telegram.TargetChatID, "#Calls")
	if err != nil {
		utils.TgSendErrorById(tgBot, cfg.Telegram.TargetChatID, 0, "Failed to create/retreive corresponding thread id for calls", err)
		return
	}

	bridgeText := fmt.Sprintf("<b>%s\n%s</b>",
		html.EscapeString(callerName), html.EscapeString(v.Timestamp.In(state.State.LocalLocation).Format(cfg.TimeFormat)))

	utils.TgSendTextById(tgBot, cfg.Telegram.TargetChatID, callThreadId, bridgeText)
}

func ReceiptEventHandler(v *events.Receipt) {
	if v.Type == events.ReceiptTypeReadSelf {
		for _, msgId := range v.MessageIDs {
			database.MsgIdMarkRead(v.Chat.String(), msgId)
		}
	}
}

func PushNameEventHandler(v *events.PushName) {
	logger := state.State.Logger
	defer logger.Sync()

	logger.Debug("new push_name update",
		zap.String("jid", v.JID.String()),
		zap.String("old_push_name", v.OldPushName),
		zap.String("new_push_name", v.NewPushName),
	)

	database.ContactUpdatePushName(v.JID.User, v.NewPushName)
}

func RevokedMessageEventHandler(v *events.Message) {
	var (
		cfg         = state.State.Config
		tgBot       = state.State.TelegramBot
		protocolMsg = v.Message.GetProtocolMessage()
		waMsgId     = protocolMsg.GetKey().GetId()
		waChatId    = v.Info.Chat.String()
	)

	if !cfg.WhatsApp.SendRevokedMessageUpdates {
		return
	}

	deleter := v.Info.MessageSource.Sender

	var deleterName string
	if v.Info.IsFromMe {
		deleterName = "you"
	} else {
		deleterName = utils.WaGetContactName(deleter)
	}

	tgChatId, tgThreadId, tgMsgId, err := database.MsgIdGetTgFromWa(waMsgId, waChatId)
	if err != nil || tgChatId == 0 || tgThreadId == 0 || tgMsgId == 0 {
		return
	}

	tgBot.SendMessage(tgChatId, fmt.Sprintf(
		"Revoked by <b>%s</b>",
		html.EscapeString(deleterName),
	), &gotgbot.SendMessageOpts{
		MessageThreadId:  tgThreadId,
		ReplyToMessageId: tgMsgId,
	})
}

func PictureEventHandler(v *events.Picture) {
	var (
		cfg      = state.State.Config
		logger   = state.State.Logger
		tgBot    = state.State.TelegramBot
		waClient = state.State.WhatsAppClient
	)
	defer logger.Sync()

	tgThreadId, threadFound, err := database.ChatThreadGetTgFromWa(v.JID.ToNonAD().String(), cfg.Telegram.TargetChatID)
	if err != nil {
		logger.Warn(
			"failed to find thread for a WhatsApp chat (handling Picture event)",
			zap.String("chat", v.JID.String()),
			zap.Error(err),
		)
		return
	}
	if !threadFound || tgThreadId == 0 {
		logger.Warn(
			"no thread found for a WhatsApp chat (handling Picture event)",
			zap.String("chat", v.JID.String()),
		)
		return
	}

	if v.JID.Server == waTypes.GroupServer {
		changer := utils.WaGetContactName(v.Author)
		if v.Remove {
			updateText := fmt.Sprintf("The profile picture was removed by %s", html.EscapeString(changer))
			err = utils.TgSendTextById(
				tgBot, cfg.Telegram.TargetChatID, tgThreadId,
				updateText,
			)
			if err != nil {
				logger.Error("failed to send message to the target chat", zap.Error(err))
				return
			}
		} else {
			pictureInfo, err := waClient.GetProfilePictureInfo(
				v.JID,
				&whatsmeow.GetProfilePictureParams{
					Preview: false,
				},
			)
			if err != nil {
				logger.Error("failed to get profile picture info", zap.Error(err), zap.String("group", v.JID.String()))
				return
			}
			if pictureInfo == nil {
				logger.Error("failed to get profile picture info, received null", zap.String("group", v.JID.String()))
				return
			}

			newPictureBytes, err := utils.DownloadFileBytesByURL(pictureInfo.URL)
			if err != nil {
				logger.Error("failed to download profile picture", zap.Error(err), zap.String("group", v.JID.String()))
				return
			}

			_, err = tgBot.SendPhoto(cfg.Telegram.TargetChatID, newPictureBytes, &gotgbot.SendPhotoOpts{
				MessageThreadId: tgThreadId,
				Caption:         fmt.Sprintf("The profile picture was updated by %s", html.EscapeString(changer)),
			})
			if err != nil {
				logger.Error("failed to send message to the group", zap.Error(err))
				return
			}
		}
	} else if v.JID.Server == waTypes.DefaultUserServer {
		if v.Remove {
			updateText := fmt.Sprintf("The profile picture was removed")
			err = utils.TgSendTextById(
				tgBot, cfg.Telegram.TargetChatID, tgThreadId,
				updateText,
			)
			if err != nil {
				logger.Error("failed to send message to the target chat", zap.Error(err))
				return
			}
		} else {
			pictureInfo, err := waClient.GetProfilePictureInfo(
				v.JID,
				&whatsmeow.GetProfilePictureParams{
					Preview: false,
				},
			)
			if err != nil {
				logger.Error("failed to get profile picture info", zap.Error(err), zap.String("group", v.JID.String()))
				return
			}
			if pictureInfo == nil {
				logger.Error("failed to get profile picture info, received null", zap.String("group", v.JID.String()))
				return
			}

			newPictureBytes, err := utils.DownloadFileBytesByURL(pictureInfo.URL)
			if err != nil {
				logger.Error("failed to download profile picture", zap.Error(err), zap.String("group", v.JID.String()))
				return
			}

			_, err = tgBot.SendPhoto(cfg.Telegram.TargetChatID, newPictureBytes, &gotgbot.SendPhotoOpts{
				MessageThreadId: tgThreadId,
				Caption:         "The profile picture was updated",
			})
			if err != nil {
				logger.Error("failed to send message to the group", zap.Error(err))
				return
			}
		}
	} else {
		logger.Warn(
			"Received Picture event for unknown JID type",
			zap.String("jid", v.JID.String()),
		)
	}
}

func GroupInfoEventHandler(v *events.GroupInfo) {
	var (
		cfg    = state.State.Config
		logger = state.State.Logger
		tgBot  = state.State.TelegramBot
	)
	defer logger.Sync()

	tgThreadId, threadFound, err := database.ChatThreadGetTgFromWa(v.JID.ToNonAD().String(), cfg.Telegram.TargetChatID)
	if err != nil {
		logger.Warn(
			"failed to find thread for a WhatsApp chat (handling GroupInfo event)",
			zap.String("chat", v.JID.String()),
			zap.Error(err),
		)
		return
	}
	if !threadFound || tgThreadId == 0 {
		logger.Warn(
			"no thread found for a WhatsApp chat (handling GroupInfo event)",
			zap.String("chat", v.JID.String()),
		)
		return
	}

	if v.Announce != nil {
		var updateText string
		if v.Announce.IsAnnounce {
			updateText = "Group settings have been changed, only admins can send messages now"
		} else {
			updateText = "Group settings have been changed, everybody can send messages now"
		}
		err = utils.TgSendTextById(tgBot, cfg.Telegram.TargetChatID, tgThreadId, updateText)
		if err != nil {
			logger.Error("failed to send message", zap.Error(err))
		}
	}

	if v.Ephemeral != nil {
		var updateText string
		if v.Ephemeral.IsEphemeral {
			updateText = "Group's auto deletion timer has been turned on:\n"
			updateText += fmt.Sprintf("Timer: %s", time.Second*time.Duration(v.Ephemeral.DisappearingTimer))
		} else {
			updateText = "Group's auto deletion timer has been disabled"
		}
		err = utils.TgSendTextById(tgBot, cfg.Telegram.TargetChatID, tgThreadId, updateText)
		if err != nil {
			logger.Error("failed to send message", zap.Error(err))
		}
	}

	if v.Delete != nil {
		updateText := "The group has been deleted"
		if v.Delete.DeleteReason != "" {
			updateText += fmt.Sprintf(
				"\nReason: <code>%s</code>",
				html.EscapeString(v.Delete.DeleteReason),
			)
		}
		err = utils.TgSendTextById(
			tgBot, cfg.Telegram.TargetChatID, tgThreadId,
			"The group has been deleted",
		)
		if err != nil {
			logger.Error("failed to send message", zap.Error(err))
		}
	}

	if len(v.Join) > 0 {
		var updateText string
		if len(v.Join) == 1 {
			newMemName := utils.WaGetContactName(v.Join[0])
			updateText = fmt.Sprintf("%s joined the group\n", html.EscapeString(newMemName))
		} else {
			updateText = "The following people joined the group:\n"
			for _, newMem := range v.Join {
				newMemName := utils.WaGetContactName(newMem)
				updateText += fmt.Sprintf("- %s\n", html.EscapeString(newMemName))
			}
		}
		if v.JoinReason != "" {
			updateText += fmt.Sprintf("\nReason: %s", html.EscapeString(v.JoinReason))
		}
		err = utils.TgSendTextById(tgBot, cfg.Telegram.TargetChatID, tgThreadId, updateText)
		if err != nil {
			logger.Error("failed to send message", zap.Error(err))
		}
	}

	if len(v.Leave) > 0 {
		var updateText string
		if len(v.Leave) == 1 {
			oldMemName := utils.WaGetContactName(v.Leave[0])
			updateText = fmt.Sprintf("%s left the group\n", html.EscapeString(oldMemName))
		} else {
			updateText = "The following people left the group:\n"
			for _, oldMem := range v.Leave {
				oldMemName := utils.WaGetContactName(oldMem)
				updateText += fmt.Sprintf("- %s\n", oldMemName)
			}
		}
		err = utils.TgSendTextById(tgBot, cfg.Telegram.TargetChatID, tgThreadId, updateText)
		if err != nil {
			logger.Error("failed to send message", zap.Error(err))
		}
	}

	if len(v.Demote) > 0 {
		var updateText string
		if len(v.Demote) == 1 {
			demotedMemName := utils.WaGetContactName(v.Demote[0])
			updateText = fmt.Sprintf("%s was demoted in the group\n", html.EscapeString(demotedMemName))
		} else {
			updateText = "The following people were demoted:\n"
			for _, demotedMem := range v.Demote {
				demotedMemName := utils.WaGetContactName(demotedMem)
				updateText += fmt.Sprintf("- %s\n", demotedMemName)
			}
		}
		err = utils.TgSendTextById(tgBot, cfg.Telegram.TargetChatID, tgThreadId, updateText)
		if err != nil {
			logger.Error("failed to send message", zap.Error(err))
		}
	}

	if len(v.Promote) > 0 {
		var updateText string
		if len(v.Promote) == 1 {
			promotedMemName := utils.WaGetContactName(v.Promote[0])
			updateText = fmt.Sprintf("%s was promoted in the group\n", html.EscapeString(promotedMemName))
		} else {
			updateText = "The following people were promoted:\n"
			for _, promotedMem := range v.Promote {
				promotedMemName := utils.WaGetContactName(promotedMem)
				updateText += fmt.Sprintf("- %s\n", promotedMemName)
			}
		}
		err = utils.TgSendTextById(tgBot, cfg.Telegram.TargetChatID, tgThreadId, updateText)
		if err != nil {
			logger.Error("failed to send message", zap.Error(err))
		}
	}

	if v.Topic != nil {
		changer := utils.WaGetContactName(v.Topic.TopicSetBy)
		updateText := fmt.Sprintf(
			"The group description was changed by <b>%s</b>:\n\n<code>%s</code>",
			html.EscapeString(changer),
			html.EscapeString(v.Topic.Topic),
		)
		err = utils.TgSendTextById(tgBot, cfg.Telegram.TargetChatID, tgThreadId, updateText)
		if err != nil {
			logger.Error("failed to send message", zap.Error(err))
		}
	}

	if v.Name != nil {
		_, err = tgBot.EditForumTopic(
			cfg.Telegram.TargetChatID, tgThreadId,
			&gotgbot.EditForumTopicOpts{
				Name: v.Name.Name,
			},
		)
		if err != nil {
			logger.Error(
				"failed to change thread name",
				zap.Error(err),
				zap.String("chat", v.JID.String()),
				zap.String("new_name", v.Name.Name),
			)
			return
		}
		changer := utils.WaGetContactName(v.Name.NameSetBy)
		updateText := fmt.Sprintf(
			"The group name was changed by <b>%s</b>:\n\n<code>%s</code>",
			html.EscapeString(changer),
			html.EscapeString(v.Name.Name),
		)
		err = utils.TgSendTextById(tgBot, cfg.Telegram.TargetChatID, tgThreadId, updateText)
		if err != nil {
			logger.Error("failed to send message", zap.Error(err))
		}
	}
}
