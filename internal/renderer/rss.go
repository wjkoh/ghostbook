package renderer

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"html"
	"strings"
	"time"
)

const maxTimelineRSSItems = 50

type rssDocument struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	AtomNS  string     `xml:"xmlns:atom,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssAtomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type rssChannel struct {
	Title         string      `xml:"title"`
	Link          string      `xml:"link"`
	Description   string      `xml:"description"`
	Language      string      `xml:"language"`
	Generator     string      `xml:"generator"`
	LastBuildDate string      `xml:"lastBuildDate,omitempty"`
	AtomLink      rssAtomLink `xml:"atom:link"`
	Items         []rssItem   `xml:"item"`
}

type rssGUID struct {
	IsPermaLink bool   `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

type rssItem struct {
	Title       string   `xml:"title"`
	Link        string   `xml:"link"`
	GUID        rssGUID  `xml:"guid"`
	PubDate     string   `xml:"pubDate,omitempty"`
	Categories  []string `xml:"category,omitempty"`
	Description string   `xml:"description"`
}

func formatRSSPubDate(iso string) string {
	trimmed := strings.TrimSpace(iso)
	if trimmed == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, trimmed); err == nil {
			return t.UTC().Format(time.RFC1123Z)
		}
	}
	return ""
}

func formatTimelineRevisionRange(startRev, endRev int32) string {
	if startRev > 0 && endRev > 0 && startRev != endRev {
		return fmt.Sprintf("r%d–r%d", startRev, endRev)
	}
	if endRev > 0 {
		return fmt.Sprintf("r%d", endRev)
	}
	return ""
}

func buildTimelineRSSDescription(m TimelineMilestoneView, base string) string {
	var b strings.Builder
	if summary := strings.TrimSpace(m.Summary); summary != "" {
		fmt.Fprintf(&b, "<p>%s</p>", html.EscapeString(summary))
	}

	var metaParts []string
	if revLabel := formatTimelineRevisionRange(m.StartRevision, m.EndRevision); revLabel != "" {
		if m.EndCommitSHA != "" {
			metaParts = append(metaParts, fmt.Sprintf(`<a href="%s/rev/%s">%s (%s)</a>`,
				html.EscapeString(base), html.EscapeString(m.EndCommitSHA),
				html.EscapeString(revLabel), html.EscapeString(m.EndCommitSHA)))
		} else {
			metaParts = append(metaParts, html.EscapeString(revLabel))
		}
	}
	if m.CommitCount == 1 {
		metaParts = append(metaParts, "1 commit")
	} else if m.CommitCount > 1 {
		metaParts = append(metaParts, fmt.Sprintf("%d commits", m.CommitCount))
	}
	if authors := strings.TrimSpace(m.Authors); authors != "" {
		metaParts = append(metaParts, html.EscapeString(authors))
	}
	if len(metaParts) > 0 {
		fmt.Fprintf(&b, "<p>%s</p>", strings.Join(metaParts, " · "))
	}

	if len(m.Chapters) > 0 {
		b.WriteString("<p>Chapters: ")
		for i, ch := range m.Chapters {
			if i > 0 {
				b.WriteString(", ")
			}
			label := strings.TrimSpace(ch.Label)
			if label == "" {
				label = ch.Slug
			}
			fmt.Fprintf(&b, `<a href="%s/chapters/%s">%s</a>`,
				html.EscapeString(base), html.EscapeString(ch.Slug), html.EscapeString(label))
		}
		b.WriteString("</p>")
	}

	return b.String()
}

// RenderTimelineRSS encodes the book's semantic editorial timeline milestones as an RSS 2.0 XML feed.
func RenderTimelineRSS(book *RenderedBook, baseURL string) ([]byte, error) {
	base := cleanDiscoveryBaseURL(baseURL)

	var entries []TimelineMilestoneView
	var lastBuildDate string
	if book != nil {
		entries = book.TimelineEntries
		if len(entries) > maxTimelineRSSItems {
			entries = entries[:maxTimelineRSSItems]
		}
		if len(entries) > 0 {
			lastBuildDate = formatRSSPubDate(entries[0].TimestampISO)
		} else if len(book.RecentCommits) > 0 {
			lastBuildDate = formatRSSPubDate(book.RecentCommits[0].TimestampISO)
		}
	}

	items := make([]rssItem, 0, len(entries))
	for idx, m := range entries {
		revLabel := formatTimelineRevisionRange(m.StartRevision, m.EndRevision)
		title := strings.TrimSpace(m.Title)
		if title == "" {
			title = "Editorial Milestone"
		}
		if revLabel != "" {
			title = revLabel + " · " + title
		}

		itemLink := base + "/timeline"
		if m.EndRevision > 0 {
			if idx >= FrontPageTimelineLimit {
				itemLink = fmt.Sprintf("%s/timeline?all=1#milestone-r%d", base, m.EndRevision)
			} else {
				itemLink = fmt.Sprintf("%s/timeline#milestone-r%d", base, m.EndRevision)
			}
		} else if m.EndCommitSHA != "" {
			itemLink = fmt.Sprintf("%s/rev/%s", base, m.EndCommitSHA)
		}

		guidVal := fmt.Sprintf("urn:ghostbook:timeline:r%d-r%d:%s", m.StartRevision, m.EndRevision, strings.TrimSpace(m.EndCommitSHA))
		if m.StartRevision == 0 && m.EndRevision == 0 && strings.TrimSpace(m.EndCommitSHA) == "" {
			guidVal = fmt.Sprintf("urn:ghostbook:timeline:id:%d", m.ID)
		}

		var categories []string
		for _, ch := range m.Chapters {
			if label := strings.TrimSpace(ch.Label); label != "" {
				categories = append(categories, label)
			} else if slug := strings.TrimSpace(ch.Slug); slug != "" {
				categories = append(categories, slug)
			}
		}

		items = append(items, rssItem{
			Title:       title,
			Link:        itemLink,
			GUID:        rssGUID{IsPermaLink: false, Value: guidVal},
			PubDate:     formatRSSPubDate(m.TimestampISO),
			Categories:  categories,
			Description: buildTimelineRSSDescription(m, base),
		})
	}

	bookTitle := book.DisplayTitle()
	doc := rssDocument{
		Version: "2.0",
		AtomNS:  "http://www.w3.org/2005/Atom",
		Channel: rssChannel{
			Title:         bookTitle + " · Editorial Timeline",
			Link:          base + "/timeline",
			Description:   fmt.Sprintf("Recent editorial milestones across %s, grouped by topic and editing session with minor copy-edits filtered out.", bookTitle),
			Language:      "en-us",
			Generator:     "Ghostbook",
			LastBuildDate: lastBuildDate,
			AtomLink: rssAtomLink{
				Href: base + "/feed.xml",
				Rel:  "self",
				Type: "application/rss+xml",
			},
			Items: items,
		},
	}

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}
