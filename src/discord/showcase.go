package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"git.handmade.network/hmn/hmn/src/assets"
	"git.handmade.network/hmn/hmn/src/config"
	"git.handmade.network/hmn/hmn/src/db"
	"git.handmade.network/hmn/hmn/src/logging"
	"git.handmade.network/hmn/hmn/src/models"
	"git.handmade.network/hmn/hmn/src/oops"
	"git.handmade.network/hmn/hmn/src/parsing"
	"github.com/google/uuid"
)

var reDiscordMessageLink = regexp.MustCompile(`https?://.+?(\s|$)`)

var errNotEnoughInfo = errors.New("Discord didn't send enough info in this event for us to do this")

func (bot *botInstance) processShowcaseMsg(ctx context.Context, msg *Message) error {
	switch msg.Type {
	case MessageTypeDefault, MessageTypeReply, MessageTypeApplicationCommand:
	default:
		return nil
	}

	didDelete, err := bot.maybeDeleteShowcaseMsg(ctx, msg)
	if err != nil {
		return err
	}
	if didDelete {
		return nil
	}

	tx, err := bot.dbConn.Begin(ctx)
	if err != nil {
		panic(err)
	}
	defer tx.Rollback(ctx)

	// save the message, maybe save its contents, and maybe make a snippet too
	newMsg, err := SaveMessageAndContents(ctx, tx, msg)
	if errors.Is(err, errNotEnoughInfo) {
		logging.ExtractLogger(ctx).Warn().
			Interface("msg", msg).
			Msg("didn't have enough info to process Discord message")
		return nil
	} else if err != nil {
		return err
	}
	if doSnippet, err := AllowedToCreateMessageSnippet(ctx, tx, newMsg.UserID); doSnippet && err == nil {
		_, err := CreateMessageSnippet(ctx, tx, msg.ID)
		if err != nil {
			return oops.New(err, "failed to create snippet in gateway")
		}
	} else if err != nil {
		return oops.New(err, "failed to check snippet permissions in gateway")
	}

	err = tx.Commit(ctx)
	if err != nil {
		return oops.New(err, "failed to commit Discord message updates")
	}

	return nil
}

func (bot *botInstance) maybeDeleteShowcaseMsg(ctx context.Context, msg *Message) (didDelete bool, err error) {
	hasGoodContent := true
	if msg.OriginalHasFields("content") && !messageHasLinks(msg.Content) {
		hasGoodContent = false
	}

	hasGoodAttachments := true
	if msg.OriginalHasFields("attachments") && len(msg.Attachments) == 0 {
		hasGoodAttachments = false
	}

	didDelete = false
	if !hasGoodContent && !hasGoodAttachments {
		didDelete = true
		err := DeleteMessage(ctx, msg.ChannelID, msg.ID)
		if err != nil {
			return false, oops.New(err, "failed to delete message")
		}

		if !msg.Author.IsBot {
			channel, err := CreateDM(ctx, msg.Author.ID)
			if err != nil {
				return false, oops.New(err, "failed to create DM channel")
			}

			err = SendMessages(ctx, bot.dbConn, MessageToSend{
				ChannelID: channel.ID,
				Req: CreateMessageRequest{
					Content: "Posts in #project-showcase are required to have either an image/video or a link. Discuss showcase content in #projects.",
				},
			})
			if err != nil {
				return false, oops.New(err, "failed to send showcase warning message")
			}
		}
	}

	return didDelete, nil
}

/*
Ensures that a Discord message is stored in the database. This function is
idempotent and can be called regardless of whether the item already exists in
the database.

This does not create snippets or do anything besides save the message itself.
*/
func SaveMessage(
	ctx context.Context,
	tx db.ConnOrTx,
	msg *Message,
) (*models.DiscordMessage, error) {
	iDiscordMessage, err := db.QueryOne(ctx, tx, models.DiscordMessage{},
		`
		SELECT $columns
		FROM handmade_discordmessage
		WHERE id = $1
		`,
		msg.ID,
	)
	if errors.Is(err, db.ErrNoMatchingRows) {
		if !msg.OriginalHasFields("author", "timestamp") {
			return nil, errNotEnoughInfo
		}

		guildID := msg.GuildID
		if guildID == nil {
			/*
				This is weird, but it can happen when we fetch messages from
				history instead of receiving it from the gateway. In this case
				we just assume it's from the HMN server.
			*/
			guildID = &config.Config.Discord.GuildID
		}

		_, err = tx.Exec(ctx,
			`
			INSERT INTO handmade_discordmessage (id, channel_id, guild_id, url, user_id, sent_at, snippet_created)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			`,
			msg.ID,
			msg.ChannelID,
			*guildID,
			msg.JumpURL(),
			msg.Author.ID,
			msg.Time(),
			false,
		)
		if err != nil {
			return nil, oops.New(err, "failed to save new discord message")
		}

		/*
			TODO(db): This is a spot where it would be really nice to be able
			to use RETURNING, and avoid this second query.
		*/
		iDiscordMessage, err = db.QueryOne(ctx, tx, models.DiscordMessage{},
			`
			SELECT $columns
			FROM handmade_discordmessage
			WHERE id = $1
			`,
			msg.ID,
		)
		if err != nil {
			panic(err)
		}
	} else if err != nil {
		return nil, oops.New(err, "failed to check for existing Discord message")
	}

	return iDiscordMessage.(*models.DiscordMessage), nil
}

/*
Processes a single Discord message, saving as much of the message's content
and attachments as allowed by our rules and user settings. Does NOT create
snippets.

Idempotent; can be called any time whether the message exists or not.
*/
func SaveMessageAndContents(
	ctx context.Context,
	tx db.ConnOrTx,
	msg *Message,
) (*models.DiscordMessage, error) {
	newMsg, err := SaveMessage(ctx, tx, msg)
	if err != nil {
		return nil, err
	}

	// Check for linked Discord user
	iDiscordUser, err := db.QueryOne(ctx, tx, models.DiscordUser{},
		`
		SELECT $columns
		FROM handmade_discorduser
		WHERE userid = $1
		`,
		newMsg.UserID,
	)
	if errors.Is(err, db.ErrNoMatchingRows) {
		return newMsg, nil
	} else if err != nil {
		return nil, oops.New(err, "failed to look up linked Discord user")
	}
	discordUser := iDiscordUser.(*models.DiscordUser)

	// We have a linked Discord account, so save the message contents (regardless of
	// whether we create a snippet or not).

	if msg.OriginalHasFields("content") {
		_, err = tx.Exec(ctx,
			`
			INSERT INTO handmade_discordmessagecontent (message_id, discord_id, last_content)
			VALUES ($1, $2, $3)
			ON CONFLICT (message_id) DO UPDATE SET
				discord_id = EXCLUDED.discord_id,
				last_content = EXCLUDED.last_content
			`,
			newMsg.ID,
			discordUser.ID,
			CleanUpMarkdown(ctx, msg.Content),
		)
	}

	// Save attachments
	if msg.OriginalHasFields("attachments") {
		for _, attachment := range msg.Attachments {
			_, err := saveAttachment(ctx, tx, &attachment, discordUser.HMNUserId, msg.ID)
			if err != nil {
				return nil, oops.New(err, "failed to save attachment")
			}
		}
	}

	// Save / delete embeds
	if msg.OriginalHasFields("embeds") {
		numSavedEmbeds, err := db.QueryInt(ctx, tx,
			`
			SELECT COUNT(*)
			FROM handmade_discordmessageembed
			WHERE message_id = $1
			`,
			msg.ID,
		)
		if err != nil {
			return nil, oops.New(err, "failed to count existing embeds")
		}
		if numSavedEmbeds == 0 {
			// No embeds yet, so save new ones
			for _, embed := range msg.Embeds {
				_, err := saveEmbed(ctx, tx, &embed, discordUser.HMNUserId, msg.ID)
				if err != nil {
					return nil, oops.New(err, "failed to save embed")
				}
			}
		} else if len(msg.Embeds) > 0 {
			// Embeds were removed from the message
			_, err := tx.Exec(ctx,
				`
				DELETE FROM handmade_discordmessageembed
				WHERE message_id = $1
				`,
				msg.ID,
			)
			if err != nil {
				return nil, oops.New(err, "failed to delete embeds")
			}
		}
	}

	return newMsg, nil
}

var discordDownloadClient = &http.Client{
	Timeout: 10 * time.Second,
}

type DiscordResourceBadStatusCode error

func downloadDiscordResource(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", oops.New(err, "failed to make Discord download request")
	}
	res, err := discordDownloadClient.Do(req)
	if err != nil {
		return nil, "", oops.New(err, "failed to fetch Discord resource data")
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || 299 < res.StatusCode {
		return nil, "", DiscordResourceBadStatusCode(fmt.Errorf("status code %d from Discord resource: %s", res.StatusCode, url))
	}

	content, err := io.ReadAll(res.Body)
	if err != nil {
		panic(err)
	}

	return content, res.Header.Get("Content-Type"), nil
}

/*
Saves a Discord attachment as an HMN asset. Idempotent; will not create an attachment
that already exists
*/
func saveAttachment(
	ctx context.Context,
	tx db.ConnOrTx,
	attachment *Attachment,
	hmnUserID int,
	discordMessageID string,
) (*models.DiscordMessageAttachment, error) {
	iexisting, err := db.QueryOne(ctx, tx, models.DiscordMessageAttachment{},
		`
		SELECT $columns
		FROM handmade_discordmessageattachment
		WHERE id = $1
		`,
		attachment.ID,
	)
	if err == nil {
		return iexisting.(*models.DiscordMessageAttachment), nil
	} else if errors.Is(err, db.ErrNoMatchingRows) {
		// this is fine, just create it
	} else {
		return nil, oops.New(err, "failed to check for existing attachment")
	}

	width := 0
	height := 0
	if attachment.Width != nil {
		width = *attachment.Width
	}
	if attachment.Height != nil {
		height = *attachment.Height
	}

	content, _, err := downloadDiscordResource(ctx, attachment.Url)
	if err != nil {
		return nil, oops.New(err, "failed to download Discord attachment")
	}

	contentType := "application/octet-stream"
	if attachment.ContentType != nil {
		contentType = *attachment.ContentType
	}

	asset, err := assets.Create(ctx, tx, assets.CreateInput{
		Content:     content,
		Filename:    attachment.Filename,
		ContentType: contentType,

		UploaderID: &hmnUserID,
		Width:      width,
		Height:     height,
	})
	if err != nil {
		return nil, oops.New(err, "failed to save asset for Discord attachment")
	}

	// TODO(db): RETURNING plz thanks
	_, err = tx.Exec(ctx,
		`
		INSERT INTO handmade_discordmessageattachment (id, asset_id, message_id)
		VALUES ($1, $2, $3)
		`,
		attachment.ID,
		asset.ID,
		discordMessageID,
	)
	if err != nil {
		return nil, oops.New(err, "failed to save Discord attachment data")
	}

	iDiscordAttachment, err := db.QueryOne(ctx, tx, models.DiscordMessageAttachment{},
		`
		SELECT $columns
		FROM handmade_discordmessageattachment
		WHERE id = $1
		`,
		attachment.ID,
	)
	if err != nil {
		return nil, oops.New(err, "failed to fetch new Discord attachment data")
	}

	return iDiscordAttachment.(*models.DiscordMessageAttachment), nil
}

// Saves an embed from Discord. NOTE: This is _not_ idempotent, so only call it
// if you do not have any embeds saved for this message yet.
func saveEmbed(
	ctx context.Context,
	tx db.ConnOrTx,
	embed *Embed,
	hmnUserID int,
	discordMessageID string,
) (*models.DiscordMessageEmbed, error) {
	isOkImageType := func(contentType string) bool {
		return strings.HasPrefix(contentType, "image/")
	}

	isOkVideoType := func(contentType string) bool {
		return strings.HasPrefix(contentType, "video/")
	}

	maybeSaveImageish := func(i EmbedImageish, contentTypeCheck func(string) bool) (*uuid.UUID, error) {
		content, contentType, err := downloadDiscordResource(ctx, *i.Url)
		if err != nil {
			var statusError DiscordResourceBadStatusCode
			if errors.As(err, &statusError) {
				return nil, nil
			} else {
				return nil, oops.New(err, "failed to save Discord embed")
			}
		}
		if contentTypeCheck(contentType) {
			in := assets.CreateInput{
				Content:     content,
				Filename:    "embed",
				ContentType: contentType,
				UploaderID:  &hmnUserID,
			}

			if i.Width != nil {
				in.Width = *i.Width
			}
			if i.Height != nil {
				in.Height = *i.Height
			}

			asset, err := assets.Create(ctx, tx, in)
			if err != nil {
				return nil, oops.New(err, "failed to create asset from embed")
			}
			return &asset.ID, nil
		}

		return nil, nil
	}

	var imageAssetId *uuid.UUID
	var videoAssetId *uuid.UUID
	var err error

	if embed.Video != nil && embed.Video.Url != nil {
		videoAssetId, err = maybeSaveImageish(embed.Video.EmbedImageish, isOkVideoType)
	} else if embed.Image != nil && embed.Image.Url != nil {
		imageAssetId, err = maybeSaveImageish(embed.Image.EmbedImageish, isOkImageType)
	} else if embed.Thumbnail != nil && embed.Thumbnail.Url != nil {
		imageAssetId, err = maybeSaveImageish(embed.Thumbnail.EmbedImageish, isOkImageType)
	}
	if err != nil {
		return nil, err
	}

	// Save the embed into the db
	// TODO(db): Insert, RETURNING
	var savedEmbedId int
	err = tx.QueryRow(ctx,
		`
		INSERT INTO handmade_discordmessageembed (title, description, url, message_id, image_id, video_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
		`,
		embed.Title,
		embed.Description,
		embed.Url,
		discordMessageID,
		imageAssetId,
		videoAssetId,
	).Scan(&savedEmbedId)
	if err != nil {
		return nil, oops.New(err, "failed to insert new embed")
	}

	iDiscordEmbed, err := db.QueryOne(ctx, tx, models.DiscordMessageEmbed{},
		`
		SELECT $columns
		FROM handmade_discordmessageembed
		WHERE id = $1
		`,
		savedEmbedId,
	)
	if err != nil {
		return nil, oops.New(err, "failed to fetch new Discord embed data")
	}

	return iDiscordEmbed.(*models.DiscordMessageEmbed), nil
}

/*
Checks settings and permissions to decide whether we are allowed to create
snippets for a user.
*/
func AllowedToCreateMessageSnippet(ctx context.Context, tx db.ConnOrTx, discordUserId string) (bool, error) {
	canSave, err := db.QueryBool(ctx, tx,
		`
		SELECT u.discord_save_showcase
		FROM
			handmade_discorduser AS duser
			JOIN auth_user AS u ON duser.hmn_user_id = u.id
		WHERE
			duser.userid = $1
		`,
		discordUserId,
	)
	if errors.Is(err, db.ErrNoMatchingRows) {
		return false, nil
	} else if err != nil {
		return false, oops.New(err, "failed to check if we can save Discord message")
	}

	return canSave, nil
}

/*
Attempts to create a snippet from a Discord message. If a snippet already
exists, it will be returned and no new snippets will be created.

It uses the content saved in the database to do this. If we do not have
any content saved, nothing will happen.

Does not check user preferences around snippets.
*/
func CreateMessageSnippet(ctx context.Context, tx db.ConnOrTx, msgID string) (*models.Snippet, error) {
	// Check for existing snippet, maybe return it
	type existingSnippetResult struct {
		Message        models.DiscordMessage         `db:"msg"`
		MessageContent *models.DiscordMessageContent `db:"c"`
		Snippet        *models.Snippet               `db:"snippet"`
		DiscordUser    *models.DiscordUser           `db:"duser"`
	}
	iexisting, err := db.QueryOne(ctx, tx, existingSnippetResult{},
		`
		SELECT $columns
		FROM
			handmade_discordmessage AS msg
			LEFT JOIN handmade_discordmessagecontent AS c ON c.message_id = msg.id
			LEFT JOIN handmade_snippet AS snippet ON snippet.discord_message_id = msg.id
			LEFT JOIN handmade_discorduser AS duser ON msg.user_id = duser.userid
		WHERE
			msg.id = $1
		`,
		msgID,
	)
	if err != nil {
		return nil, oops.New(err, "failed to check for existing snippet for message %s", msgID)
	}
	existing := iexisting.(*existingSnippetResult)

	if existing.Snippet != nil {
		// A snippet already exists - maybe update its content, then return it
		if existing.MessageContent != nil && !existing.Snippet.EditedOnWebsite {
			contentMarkdown := existing.MessageContent.LastContent
			contentHTML := parsing.ParseMarkdown(contentMarkdown, parsing.DiscordMarkdown)

			_, err := tx.Exec(ctx,
				`
				UPDATE handmade_snippet
				SET
					description = $1,
					_description_html = $2
				WHERE id = $3
				`,
				contentMarkdown,
				contentHTML,
				existing.Snippet.ID,
			)
			if err != nil {
				logging.ExtractLogger(ctx).Warn().Err(err).Msg("failed to update content of snippet on message edit")
			}
		}
		return existing.Snippet, nil
	}

	if existing.Message.SnippetCreated {
		// A snippet once existed but no longer does
		// (we do not create another one in this case)
		return nil, nil
	}

	if existing.MessageContent == nil || existing.DiscordUser == nil {
		return nil, nil
	}

	// Get an asset ID or URL to make a snippet from
	assetId, url, err := getSnippetAssetOrUrl(ctx, tx, &existing.Message)
	if assetId == nil && url == nil {
		// Nothing to make a snippet from!
		return nil, nil
	}

	contentMarkdown := existing.MessageContent.LastContent
	contentHTML := parsing.ParseMarkdown(contentMarkdown, parsing.DiscordMarkdown)

	// TODO(db): Insert
	isnippet, err := db.QueryOne(ctx, tx, models.Snippet{},
		`
		INSERT INTO handmade_snippet (url, "when", description, _description_html, asset_id, discord_message_id, owner_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING $columns
		`,
		url,
		existing.Message.SentAt,
		contentMarkdown,
		contentHTML,
		assetId,
		msgID,
		existing.DiscordUser.HMNUserId,
	)
	if err != nil {
		return nil, oops.New(err, "failed to create snippet from attachment")
	}
	_, err = tx.Exec(ctx,
		`
		UPDATE handmade_discordmessage
		SET snippet_created = TRUE
		WHERE id = $1
		`,
		msgID,
	)
	if err != nil {
		return nil, oops.New(err, "failed to mark message as having snippet")
	}

	return isnippet.(*models.Snippet), nil
}

// NOTE(ben): This is maybe redundant with the regexes we use for markdown. But
// do we actually want to reuse those, or should we keep them separate?
var RESnippetableUrl = regexp.MustCompile(`^https?://(youtu\.be|(www\.)?youtube\.com/watch)`)

func getSnippetAssetOrUrl(ctx context.Context, tx db.ConnOrTx, msg *models.DiscordMessage) (*uuid.UUID, *string, error) {
	// Check attachments
	itAttachments, err := db.Query(ctx, tx, models.DiscordMessageAttachment{},
		`
		SELECT $columns
		FROM handmade_discordmessageattachment
		WHERE message_id = $1
		`,
		msg.ID,
	)
	if err != nil {
		return nil, nil, oops.New(err, "failed to fetch message attachments")
	}
	attachments := itAttachments.ToSlice()
	for _, iattachment := range attachments {
		attachment := iattachment.(*models.DiscordMessageAttachment)
		return &attachment.AssetID, nil, nil
	}

	// Check embeds
	itEmbeds, err := db.Query(ctx, tx, models.DiscordMessageEmbed{},
		`
		SELECT $columns
		FROM handmade_discordmessageembed
		WHERE message_id = $1
		`,
		msg.ID,
	)
	if err != nil {
		return nil, nil, oops.New(err, "failed to fetch discord embeds")
	}
	embeds := itEmbeds.ToSlice()
	for _, iembed := range embeds {
		embed := iembed.(*models.DiscordMessageEmbed)
		if embed.VideoID != nil {
			return embed.VideoID, nil, nil
		} else if embed.ImageID != nil {
			return embed.ImageID, nil, nil
		} else if embed.URL != nil {
			if RESnippetableUrl.MatchString(*embed.URL) {
				return nil, embed.URL, nil
			}
		}
	}

	return nil, nil, nil
}

func messageHasLinks(content string) bool {
	links := reDiscordMessageLink.FindAllString(content, -1)
	for _, link := range links {
		_, err := url.Parse(strings.TrimSpace(link))
		if err == nil {
			return true
		}
	}

	return false
}
