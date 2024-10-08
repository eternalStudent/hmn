package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"git.handmade.network/hmn/hmn/src/config"
	"git.handmade.network/hmn/hmn/src/db"
	"git.handmade.network/hmn/hmn/src/hmndata"
	"git.handmade.network/hmn/hmn/src/logging"
	"git.handmade.network/hmn/hmn/src/models"
	"git.handmade.network/hmn/hmn/src/oops"
)

// NOTE(asaf): Updates or creates a discord message according to the following rules:
//
//	            Create when:
//	              * No previous message exists
//	              * We have non-zero live streamers
//	              * Message exists, but we're adding a new streamer that wasn't in the existing message
//	              * Message exists, but is not the most recent message in the channel
//				   Update otherwise
//	            That way we ensure that the message doesn't get scrolled offscreen, and the
//	            new message indicator for the channel doesn't trigger when a streamer goes offline or
//	            updates the stream title.
//
// NOTE(asaf): No-op if StreamsChannelID is not specified in the config
func UpdateStreamers(ctx context.Context, dbConn db.ConnOrTx, streamers []hmndata.StreamDetails) error {
	if len(config.Config.Discord.StreamsChannelID) == 0 {
		return nil
	}

	livestreamMessage, err := hmndata.FetchPersistentVar[hmndata.DiscordLivestreamMessage](
		ctx,
		dbConn,
		hmndata.VarNameDiscordLivestreamMessage,
	)
	editExisting := true
	if err != nil {
		if err == db.NotFound {
			editExisting = false
		} else {
			return oops.New(err, "failed to fetch last message persistent var from db")
		}
	}

	if editExisting {
		// Make sure we have a message to edit
		_, err := GetChannelMessage(ctx, config.Config.Discord.StreamsChannelID, livestreamMessage.MessageID)
		if err != nil {
			if err == NotFound {
				editExisting = false
			} else {
				oops.New(err, "failed to fetch existing message from discord")
			}
		}
	}

	if len(streamers) == 0 {
		if livestreamMessage != nil {
			err = DeleteMessage(ctx, config.Config.Discord.StreamsChannelID, livestreamMessage.MessageID)
			if err != nil {
				return oops.New(err, "failed to delete livestream message from discord")
			}
			err = hmndata.RemovePersistentVar(ctx, dbConn, hmndata.VarNameDiscordLivestreamMessage)
			if err != nil {
				return oops.New(err, "failed to clear discord persistent var")
			}
		}
	} else {
		if editExisting {
			// Check if we have new streamers to add
			existingStreamers := livestreamMessage.Streamers
			for _, s := range streamers {
				found := false
				for _, es := range existingStreamers {
					if es.Username == s.Username {
						found = true
						break
					}
				}
				if !found {
					editExisting = false
					break
				}
			}
		}

		if editExisting {
			// Check that our editable message is the latest in the channel
			messages, err := GetChannelMessages(ctx, config.Config.Discord.StreamsChannelID, GetChannelMessagesInput{
				Limit: 1,
			})
			if err != nil {
				return oops.New(err, "failed to fetch messages from discord")
			}
			if len(messages) == 0 || messages[0].ID != livestreamMessage.MessageID {
				editExisting = false
			}
		}

		messageContent := ""
		var builder strings.Builder
		for _, s := range streamers {
			builder.WriteString(fmt.Sprintf(":red_circle: **%s** is live: <https://twitch.tv/%s>\n> _%s_\nStarted <t:%d:R>\n\n", s.Username, s.Username, s.Title, s.StartTime.Unix()))
		}
		messageContent = builder.String()

		msgJson, err := json.Marshal(CreateMessageRequest{
			Content:         messageContent,
			Flags:           FlagSuppressEmbeds,
			AllowedMentions: &MessageAllowedMentions{},
		})
		if err != nil {
			return oops.New(err, "failed to marshal discord message")
		}

		newMessageID := ""
		if editExisting {
			updatedMessage, err := EditMessage(ctx, config.Config.Discord.StreamsChannelID, livestreamMessage.MessageID, string(msgJson))
			if err != nil {
				return oops.New(err, "failed to update discord message for streams channel")
			}

			newMessageID = updatedMessage.ID
		} else {
			if livestreamMessage != nil {
				err = DeleteMessage(ctx, config.Config.Discord.StreamsChannelID, livestreamMessage.MessageID)
				if err != nil {
					log := logging.ExtractLogger(ctx)
					log.Error().Err(err).Msg("failed to delete existing discord message from streams channel")
				}
			}

			sentMessage, err := CreateMessage(ctx, config.Config.Discord.StreamsChannelID, string(msgJson))
			if err != nil {
				return oops.New(err, "failed to create discord message for streams channel")
			}

			newMessageID = sentMessage.ID
		}

		data := hmndata.DiscordLivestreamMessage{
			MessageID: newMessageID,
			Streamers: streamers,
		}
		err = hmndata.StorePersistentVar(ctx, dbConn, hmndata.VarNameDiscordLivestreamMessage, &data)
		if err != nil {
			return oops.New(err, "failed to store persistent var for discord streams")
		}

	}
	return nil
}

func PostStreamHistory(ctx context.Context, history *models.TwitchStreamHistory) (string, error) {
	if len(config.Config.Discord.StreamsChannelID) == 0 {
		return "", nil
	}

	approximated := ""
	if history.EndApproximated {
		approximated = "about "
	}
	duration := history.EndedAt.Sub(history.StartedAt).Truncate(time.Second).String()
	messageContent := fmt.Sprintf(
		":o: **%s** was live: https://twitch.tv/%s\n> _%s_\nOn <t:%d:F> for %s%s",
		history.TwitchLogin,
		history.TwitchLogin,
		history.Title,
		history.StartedAt.Unix(),
		approximated,
		duration,
	)
	if history.VODUrl != "" {
		messageContent += fmt.Sprintf("\nVOD: %s", history.VODUrl)
	}
	msgJson, err := json.Marshal(CreateMessageRequest{
		Content:         messageContent,
		Flags:           FlagSuppressEmbeds,
		AllowedMentions: &MessageAllowedMentions{},
	})
	if err != nil {
		return "", oops.New(err, "failed to marshal discord message")
	}
	messageID := ""
	if history.DiscordMessageID != "" {
		updatedMessage, err := EditMessage(ctx, config.Config.Discord.StreamsChannelID, history.DiscordMessageID, string(msgJson))
		if err != nil {
			return "", oops.New(err, "failed to update discord message for stream history")
		}
		messageID = updatedMessage.ID
	} else {
		msg, err := CreateMessage(ctx, config.Config.Discord.StreamsChannelID, string(msgJson))
		if err != nil {
			return "", oops.New(err, "failed to create discord message for stream history")
		}
		messageID = msg.ID
	}
	return messageID, nil
}
