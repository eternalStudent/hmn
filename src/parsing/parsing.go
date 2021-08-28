package parsing

import (
	"bytes"

	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/util"
)

// Used for rendering real-time previews of post content.
var ForumPreviewMarkdown = goldmark.New(
	goldmark.WithExtensions(makeGoldmarkExtensions(MarkdownOptions{
		Previews: true,
		Embeds:   true,
	})...),
)

// Used for generating the final HTML for a post.
var ForumRealMarkdown = goldmark.New(
	goldmark.WithExtensions(makeGoldmarkExtensions(MarkdownOptions{
		Previews: false,
		Embeds:   true,
	})...),
)

// Used for generating plain-text previews of posts.
var PlaintextMarkdown = goldmark.New(
	goldmark.WithExtensions(makeGoldmarkExtensions(MarkdownOptions{
		Previews: false,
		Embeds:   true,
	})...),
	goldmark.WithRenderer(plaintextRenderer{}),
)

// Used for processing Discord messages
var DiscordMarkdown = goldmark.New(
	goldmark.WithExtensions(makeGoldmarkExtensions(MarkdownOptions{
		Previews: false,
		Embeds:   false,
	})...),
)

func ParseMarkdown(source string, md goldmark.Markdown) string {
	var buf bytes.Buffer
	if err := md.Convert([]byte(source), &buf); err != nil {
		panic(err)
	}

	return buf.String()
}

type MarkdownOptions struct {
	Previews bool
	Embeds   bool
}

func makeGoldmarkExtensions(opts MarkdownOptions) []goldmark.Extender {
	var extenders []goldmark.Extender
	extenders = append(extenders,
		extension.GFM,
		highlightExtension,
		SpoilerExtension{},
	)

	if opts.Embeds {
		extenders = append(extenders,
			EmbedExtension{
				Preview: opts.Previews,
			},
		)
	}

	extenders = append(extenders,
		MathjaxExtension{},
		BBCodeExtension{
			Preview: opts.Previews,
		},
	)

	return extenders
}

var highlightExtension = highlighting.NewHighlighting(
	highlighting.WithFormatOptions(HMNChromaOptions...),
	highlighting.WithWrapperRenderer(func(w util.BufWriter, context highlighting.CodeBlockContext, entering bool) {
		if entering {
			w.WriteString(`<pre class="hmn-code">`)
		} else {
			w.WriteString(`</pre>`)
		}
	}),
)
