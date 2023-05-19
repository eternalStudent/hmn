package website

import (
	"fmt"
	"html/template"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"git.handmade.network/hmn/hmn/src/hmndata"
	"git.handmade.network/hmn/hmn/src/hmnurl"
	"git.handmade.network/hmn/hmn/src/logging"
	"git.handmade.network/hmn/hmn/src/models"
	"git.handmade.network/hmn/hmn/src/templates"
)

type TimelineTypeTitles struct {
	TypeTitleFirst    string
	TypeTitleNotFirst string
	FilterTitle       string
}

var TimelineTypeTitleMap = map[models.ThreadType]TimelineTypeTitles{
	models.ThreadTypeProjectBlogPost: {"New blog post", "Blog comment", "Blogs"},
	models.ThreadTypeForumPost:       {"New forum thread", "Forum reply", "Forums"},
}

func PostToTimelineItem(
	urlContext *hmnurl.UrlContext,
	lineageBuilder *models.SubforumLineageBuilder,
	post *models.Post,
	thread *models.Thread,
	owner *models.User,
	currentTheme string,
) templates.TimelineItem {
	item := templates.TimelineItem{
		Date:        post.PostDate,
		Title:       thread.Title,
		Breadcrumbs: GenericThreadBreadcrumbs(urlContext, lineageBuilder, thread),
		Url:         UrlForGenericPost(urlContext, thread, post, lineageBuilder),

		OwnerAvatarUrl: templates.UserAvatarUrl(owner, currentTheme),
		OwnerName:      owner.BestName(),
		OwnerUrl:       hmnurl.BuildUserProfile(owner.Username),
	}

	if typeTitles, ok := TimelineTypeTitleMap[post.ThreadType]; ok {
		if thread.FirstID == post.ID {
			item.TypeTitle = typeTitles.TypeTitleFirst
		} else {
			item.TypeTitle = typeTitles.TypeTitleNotFirst
		}
		item.FilterTitle = typeTitles.FilterTitle
	} else {
		logging.Warn().
			Int("postID", post.ID).
			Int("threadType", int(post.ThreadType)).
			Msg("unknown thread type for post")
	}

	return item
}

func SnippetToTimelineItem(
	snippet *models.Snippet,
	asset *models.Asset,
	discordMessage *models.DiscordMessage,
	projects []*hmndata.ProjectAndStuff,
	owner *models.User,
	currentTheme string,
	editable bool,
) templates.TimelineItem {
	item := templates.TimelineItem{
		ID:          strconv.Itoa(snippet.ID),
		Date:        snippet.When,
		FilterTitle: "Snippets",
		Url:         hmnurl.BuildSnippet(snippet.ID),

		OwnerAvatarUrl: templates.UserAvatarUrl(owner, currentTheme),
		OwnerName:      owner.BestName(),
		OwnerUrl:       hmnurl.BuildUserProfile(owner.Username),

		Description:    template.HTML(snippet.DescriptionHtml),
		RawDescription: snippet.Description,

		CanShowcase: true,
		Editable:    editable,
	}

	if asset != nil {
		if strings.HasPrefix(asset.MimeType, "image/") {
			item.EmbedMedia = append(item.EmbedMedia, imageMediaItem(asset))
		} else if strings.HasPrefix(asset.MimeType, "video/") {
			item.EmbedMedia = append(item.EmbedMedia, videoMediaItem(asset))
		} else if strings.HasPrefix(asset.MimeType, "audio/") {
			item.EmbedMedia = append(item.EmbedMedia, audioMediaItem(asset))
		} else {
			item.EmbedMedia = append(item.EmbedMedia, unknownMediaItem(asset))
		}
	}

	if snippet.Url != nil {
		url := *snippet.Url
		if videoId := getYoutubeVideoID(url); videoId != "" {
			item.EmbedMedia = append(item.EmbedMedia, youtubeMediaItem(videoId))
			item.CanShowcase = false
		}
	}

	if len(item.EmbedMedia) == 0 ||
		(len(item.EmbedMedia) > 0 && (item.EmbedMedia[0].Width == 0 || item.EmbedMedia[0].Height == 0)) {
		item.CanShowcase = false
	}

	if discordMessage != nil {
		item.DiscordMessageUrl = discordMessage.Url
	}

	sort.Slice(projects, func(i, j int) bool {
		return projects[i].Project.Name < projects[j].Project.Name
	})
	for _, proj := range projects {
		item.Projects = append(item.Projects, templates.ProjectAndStuffToTemplate(proj, hmndata.UrlContextForProject(&proj.Project).BuildHomepage(), currentTheme))
	}

	return item
}

var youtubeRegexes = [...]*regexp.Regexp{
	regexp.MustCompile(`(?i)youtube\.com/watch\?.*v=(?P<videoid>[^/&]+)`),
	regexp.MustCompile(`(?i)youtu\.be/(?P<videoid>[^/]+)`),
}

func getYoutubeVideoID(url string) string {
	for _, regex := range youtubeRegexes {
		match := regex.FindStringSubmatch(url)
		if match != nil {
			return match[regex.SubexpIndex("videoid")]
		}
	}

	return ""
}

func imageMediaItem(asset *models.Asset) templates.TimelineItemMedia {
	assetUrl := hmnurl.BuildS3Asset(asset.S3Key)

	return templates.TimelineItemMedia{
		Type:         templates.TimelineItemMediaTypeImage,
		AssetUrl:     assetUrl,
		ThumbnailUrl: assetUrl, // TODO: Use smaller thumbnails?
		MimeType:     asset.MimeType,
		Width:        asset.Width,
		Height:       asset.Height,
	}
}

func videoMediaItem(asset *models.Asset) templates.TimelineItemMedia {
	assetUrl := hmnurl.BuildS3Asset(asset.S3Key)
	var thumbnailUrl string
	if asset.ThumbnailS3Key != "" {
		thumbnailUrl = hmnurl.BuildS3Asset(asset.ThumbnailS3Key)
	}

	return templates.TimelineItemMedia{
		Type:         templates.TimelineItemMediaTypeVideo,
		AssetUrl:     assetUrl,
		ThumbnailUrl: thumbnailUrl,
		MimeType:     asset.MimeType,
		Width:        asset.Width,
		Height:       asset.Height,
	}
}

func audioMediaItem(asset *models.Asset) templates.TimelineItemMedia {
	assetUrl := hmnurl.BuildS3Asset(asset.S3Key)

	return templates.TimelineItemMedia{
		Type:     templates.TimelineItemMediaTypeAudio,
		AssetUrl: assetUrl,
		MimeType: asset.MimeType,
		Width:    asset.Width,
		Height:   asset.Height,
	}
}

func youtubeMediaItem(videoId string) templates.TimelineItemMedia {
	return templates.TimelineItemMedia{
		Type: templates.TimelineItemMediaTypeEmbed,
		EmbedHTML: template.HTML(fmt.Sprintf(
			`<iframe src="https://www.youtube-nocookie.com/embed/%s" allow="accelerometer; encrypted-media; gyroscope;" allowfullscreen frameborder="0"></iframe>`,
			template.HTMLEscapeString(videoId),
		)),
		ExtraOpenGraphItems: []templates.OpenGraphItem{
			{Property: "og:video", Value: fmt.Sprintf("https://youtube.com/watch?v=%s", videoId)},
			{Name: "twitter:card", Value: "player"},
		},
	}
}

func unknownMediaItem(asset *models.Asset) templates.TimelineItemMedia {
	assetUrl := hmnurl.BuildS3Asset(asset.S3Key)

	return templates.TimelineItemMedia{
		Type:     templates.TimelineItemMediaTypeUnknown,
		AssetUrl: assetUrl,
		MimeType: asset.MimeType,
		Filename: asset.Filename,
		FileSize: asset.Size,
	}
}
