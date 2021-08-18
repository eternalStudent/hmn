package website

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"git.handmade.network/hmn/hmn/src/db"
	"git.handmade.network/hmn/hmn/src/models"
	"git.handmade.network/hmn/hmn/src/oops"
	"git.handmade.network/hmn/hmn/src/templates"
)

type UserProfileTemplateData struct {
	templates.BaseData
	ProfileUser         templates.User
	ProfileUserLinks    []templates.Link
	ProfileUserProjects []templates.Project
	TimelineItems       []templates.TimelineItem
	NumForums           int
	NumBlogs            int
	NumSnippets         int
}

func UserProfile(c *RequestContext) ResponseData {
	username, hasUsername := c.PathParams["username"]

	if !hasUsername || len(strings.TrimSpace(username)) == 0 {
		return FourOhFour(c)
	}

	username = strings.ToLower(username)

	var profileUser *models.User
	if c.CurrentUser != nil && strings.ToLower(c.CurrentUser.Username) == username {
		profileUser = c.CurrentUser
	} else {
		c.Perf.StartBlock("SQL", "Fetch user")
		userResult, err := db.QueryOne(c.Context(), c.Conn, models.User{},
			`
			SELECT $columns
			FROM
				auth_user
			WHERE
				LOWER(auth_user.username) = $1
			`,
			username,
		)
		c.Perf.EndBlock()
		if err != nil {
			if errors.Is(err, db.ErrNoMatchingRows) {
				return FourOhFour(c)
			} else {
				return ErrorResponse(http.StatusInternalServerError, oops.New(err, "failed to fetch user: %s", username))
			}
		}
		profileUser = userResult.(*models.User)
	}
	c.Perf.StartBlock("SQL", "Fetch user links")
	type userLinkQuery struct {
		UserLink models.Link `db:"link"`
	}
	userLinkQueryResult, err := db.Query(c.Context(), c.Conn, userLinkQuery{},
		`
		SELECT $columns
		FROM
			handmade_links as link
		WHERE
			link.user_id = $1
		ORDER BY link.ordering ASC
		`,
		profileUser.ID,
	)
	if err != nil {
		return ErrorResponse(http.StatusInternalServerError, oops.New(err, "failed to fetch links for user: %s", username))
	}
	userLinksSlice := userLinkQueryResult.ToSlice()
	profileUserLinks := make([]templates.Link, 0, len(userLinksSlice))
	for _, l := range userLinksSlice {
		profileUserLinks = append(profileUserLinks, templates.LinkToTemplate(&l.(*userLinkQuery).UserLink))
	}
	c.Perf.EndBlock()

	type projectQuery struct {
		Project models.Project `db:"project"`
	}
	c.Perf.StartBlock("SQL", "Fetch projects")
	projectQueryResult, err := db.Query(c.Context(), c.Conn, projectQuery{},
		`
		SELECT $columns
		FROM
			handmade_project AS project
			INNER JOIN handmade_user_projects AS uproj ON uproj.project_id = project.id
		WHERE
			uproj.user_id = $1
			AND ($2 OR (project.flags = 0 AND project.lifecycle = ANY ($3)))
		`,
		profileUser.ID,
		(c.CurrentUser != nil && (profileUser == c.CurrentUser || c.CurrentUser.IsStaff)),
		models.VisibleProjectLifecycles,
	)
	if err != nil {
		return ErrorResponse(http.StatusInternalServerError, oops.New(err, "failed to fetch projects for user: %s", username))
	}
	projectQuerySlice := projectQueryResult.ToSlice()
	templateProjects := make([]templates.Project, 0, len(projectQuerySlice))
	for _, projectRow := range projectQuerySlice {
		projectData := projectRow.(*projectQuery)
		templateProjects = append(templateProjects, templates.ProjectToTemplate(&projectData.Project, c.Theme))
	}
	c.Perf.EndBlock()

	type postQuery struct {
		Post    models.Post    `db:"post"`
		Thread  models.Thread  `db:"thread"`
		Project models.Project `db:"project"`
	}
	c.Perf.StartBlock("SQL", "Fetch posts")
	postQueryResult, err := db.Query(c.Context(), c.Conn, postQuery{},
		`
		SELECT $columns
		FROM
			handmade_post AS post
			INNER JOIN handmade_thread AS thread ON thread.id = post.thread_id
			INNER JOIN handmade_project AS project ON project.id = post.project_id
		WHERE
			post.author_id = $1
			AND project.lifecycle = ANY ($2)
		`,
		profileUser.ID,
		models.VisibleProjectLifecycles,
	)
	if err != nil {
		return ErrorResponse(http.StatusInternalServerError, oops.New(err, "failed to fetch posts for user: %s", username))
	}
	postQuerySlice := postQueryResult.ToSlice()
	c.Perf.EndBlock()

	type snippetQuery struct {
		Snippet        models.Snippet         `db:"snippet"`
		Asset          *models.Asset          `db:"asset"`
		DiscordMessage *models.DiscordMessage `db:"discord_message"`
	}
	c.Perf.StartBlock("SQL", "Fetch snippets")
	snippetQueryResult, err := db.Query(c.Context(), c.Conn, snippetQuery{},
		`
		SELECT $columns
		FROM
			handmade_snippet AS snippet
			LEFT JOIN handmade_asset AS asset ON asset.id = snippet.asset_id
			LEFT JOIN handmade_discordmessage AS discord_message ON discord_message.id = snippet.discord_message_id
		WHERE
			snippet.owner_id = $1
		`,
		profileUser.ID,
	)
	if err != nil {
		return ErrorResponse(http.StatusInternalServerError, oops.New(err, "failed to fetch snippets for user: %s", username))
	}
	snippetQuerySlice := snippetQueryResult.ToSlice()
	c.Perf.EndBlock()

	c.Perf.StartBlock("SQL", "Fetch subforum tree")
	subforumTree := models.GetFullSubforumTree(c.Context(), c.Conn)
	lineageBuilder := models.MakeSubforumLineageBuilder(subforumTree)
	c.Perf.EndBlock()

	c.Perf.StartBlock("PROFILE", "Construct timeline items")
	timelineItems := make([]templates.TimelineItem, 0, len(postQuerySlice)+len(snippetQuerySlice))
	numForums := 0
	numBlogs := 0
	numSnippets := len(snippetQuerySlice)

	for _, postRow := range postQuerySlice {
		postData := postRow.(*postQuery)
		timelineItem := PostToTimelineItem(
			lineageBuilder,
			&postData.Post,
			&postData.Thread,
			&postData.Project,
			profileUser,
			c.Theme,
		)
		switch timelineItem.Type {
		case templates.TimelineTypeForumThread, templates.TimelineTypeForumReply:
			numForums += 1
		case templates.TimelineTypeBlogPost, templates.TimelineTypeBlogComment:
			numBlogs += 1
		}
		if timelineItem.Type != templates.TimelineTypeUnknown {
			timelineItems = append(timelineItems, timelineItem)
		} else {
			c.Logger.Warn().Int("post ID", postData.Post.ID).Msg("Unknown timeline item type for post")
		}
	}

	for _, snippetRow := range snippetQuerySlice {
		snippetData := snippetRow.(*snippetQuery)
		timelineItem := SnippetToTimelineItem(
			&snippetData.Snippet,
			snippetData.Asset,
			snippetData.DiscordMessage,
			profileUser,
			c.Theme,
		)
		timelineItems = append(timelineItems, timelineItem)
	}

	c.Perf.StartBlock("PROFILE", "Sort timeline")
	sort.Slice(timelineItems, func(i, j int) bool {
		return timelineItems[j].Date.Before(timelineItems[i].Date)
	})
	c.Perf.EndBlock()

	c.Perf.EndBlock()

	templateUser := templates.UserToTemplate(profileUser, c.Theme)

	baseData := getBaseData(c)
	baseData.Title = templateUser.Name

	var res ResponseData
	res.MustWriteTemplate("user_profile.html", UserProfileTemplateData{
		BaseData:            baseData,
		ProfileUser:         templateUser,
		ProfileUserLinks:    profileUserLinks,
		ProfileUserProjects: templateProjects,
		TimelineItems:       timelineItems,
		NumForums:           numForums,
		NumBlogs:            numBlogs,
		NumSnippets:         numSnippets,
	}, c.Perf)
	return res
}
