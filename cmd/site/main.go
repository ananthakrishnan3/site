package main

import (
	"context"
	"html/template"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Xe/jsonfeed"
	"github.com/Xe/ln"
	"github.com/gorilla/feeds"
	blackfriday "github.com/russross/blackfriday"
	"github.com/tj/front"
	analytics "gopkg.in/segmentio/analytics-go.v3"
)

var port = os.Getenv("PORT")

func main() {
	if port == "" {
		port = "29384"
	}

	s, err := Build()
	if err != nil {
		ln.FatalErr(context.Background(), err, ln.Action("Build"))
	}

	ln.Log(context.Background(), ln.F{"action": "http_listening", "port": port})
	http.ListenAndServe(":"+port, s)
}

// Site is the parent object for https://christine.website's backend.
type Site struct {
	Posts  Posts
	Resume template.HTML

	rssFeed  *feeds.Feed
	jsonFeed *jsonfeed.Feed

	mux *http.ServeMux

	segment analytics.Client
	t       *translations
}

func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ln.Log(r.Context(), ln.F{"action": "Site.ServeHTTP", "user_ip_address": r.RemoteAddr, "path": r.RequestURI})

	s.mux.ServeHTTP(w, r)

	if s.segment != nil {
		if !strings.HasPrefix(r.RequestURI, "/blog/") {
			err := s.segment.Enqueue(analytics.Track{
				UserId:     Hash("h", r.Header.Get("X-Forwarded-For")),
				Event:      "Page Viewed",
				Properties: analytics.NewProperties().SetURL(r.RequestURI),
			})
			if err != nil {
				ln.Error(r.Context(), err)
			}
		}
	}
}

// Build creates a new Site instance or fails.
func Build() (*Site, error) {
	type postFM struct {
		Title string
		Date  string
	}

	t := &translations{
		locales: map[string]locale{},
	}

	for _, lang := range []string{"en", "tp"} {
		fin, err := os.Open(filepath.Join("locales", lang+".json"))
		if err != nil {
			return nil, err
		}
		defer fin.Close()

		err = t.LoadLocale(lang, fin)
		if err != nil {
			return nil, err
		}
	}

	l := t.locales["en"]

	s := &Site{
		rssFeed: &feeds.Feed{
			Title:       l.Value("blog", "title"),
			Link:        &feeds.Link{Href: "https://christine.website/blog"},
			Description: l.Value("blog", "description"),
			Author:      &feeds.Author{Name: "Christine Dodrill", Email: "me@christine.website"},
			Created:     bootTime,
			Copyright:   l.Value("meta", "rss_copyright"),
		},
		jsonFeed: &jsonfeed.Feed{
			Version:     jsonfeed.CurrentVersion,
			Title:       l.Value("blog", "title"),
			HomePageURL: "https://christine.website",
			FeedURL:     "https://christine.website/blog.json",
			Description: l.Value("blog", "description"),
			UserComment: l.Value("meta", "json_feed"),
			Icon:        icon,
			Favicon:     icon,
			Author: jsonfeed.Author{
				Name:   l.Value("header", "name"),
				Avatar: icon,
			},
		},
		mux: http.NewServeMux(),
	}

	if wk := os.Getenv("SEGMENT_WRITE_KEY"); wk != "" {
		s.segment = analytics.New(wk)
	}

	err := filepath.Walk("./blog/", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		fin, err := os.Open(path)
		if err != nil {
			return err
		}
		defer fin.Close()

		content, err := ioutil.ReadAll(fin)
		if err != nil {
			return err
		}

		var fm postFM
		remaining, err := front.Unmarshal(content, &fm)
		if err != nil {
			return err
		}

		output := blackfriday.Run(remaining)

		p := &Post{
			Title:    fm.Title,
			Date:     fm.Date,
			Link:     strings.Split(path, ".")[0],
			Body:     string(remaining),
			BodyHTML: template.HTML(output),
		}

		s.Posts = append(s.Posts, p)

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Sort(sort.Reverse(s.Posts))

	resumeData, err := ioutil.ReadFile("./static/resume/resume.md")
	if err != nil {
		return nil, err
	}

	s.Resume = template.HTML(blackfriday.Run(resumeData))

	for _, item := range s.Posts {
		itime, _ := time.Parse("2006-01-02", item.Date)
		s.rssFeed.Items = append(s.rssFeed.Items, &feeds.Item{
			Title:       item.Title,
			Link:        &feeds.Link{Href: "https://christine.website/" + item.Link},
			Description: item.Summary,
			Created:     itime,
		})

		s.jsonFeed.Items = append(s.jsonFeed.Items, jsonfeed.Item{
			ID:            "https://christine.website/" + item.Link,
			URL:           "https://christine.website/" + item.Link,
			Title:         item.Title,
			DatePublished: itime,
			ContentHTML:   string(item.BodyHTML),
		})
	}

	// Add HTTP routes here
	s.mux.Handle("/", s.renderTemplatePage("index.html", nil))
	s.mux.Handle("/resume", s.renderTemplatePage("resume.html", s.Resume))
	s.mux.Handle("/blog", s.renderTemplatePage("blogindex.html", s.Posts))
	s.mux.Handle("/contact", s.renderTemplatePage("contact.html", nil))
	s.mux.HandleFunc("/blog.rss", s.createFeed)
	s.mux.HandleFunc("/blog.atom", s.createAtom)
	s.mux.HandleFunc("/blog.json", s.createJsonFeed)
	s.mux.HandleFunc("/blog/", s.showPost)
	s.mux.Handle("/css/", http.FileServer(http.Dir(".")))
	s.mux.Handle("/static/", http.FileServer(http.Dir(".")))
	s.mux.HandleFunc("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./static/js/sw.js")
	})

	return s, nil
}

const icon = "https://christine.website/static/img/avatar.png"

// Post is a single blogpost.
type Post struct {
	Title    string        `json:"title"`
	Link     string        `json:"link"`
	Summary  string        `json:"summary,omitifempty"`
	Body     string        `json:"-"`
	BodyHTML template.HTML `json:"body"`
	Date     string        `json:"date"`
}

// Posts implements sort.Interface for a slice of Post objects.
type Posts []*Post

func (p Posts) Len() int { return len(p) }
func (p Posts) Less(i, j int) bool {
	iDate, _ := time.Parse("2006-01-02", p[i].Date)
	jDate, _ := time.Parse("2006-01-02", p[j].Date)

	return iDate.Unix() < jDate.Unix()
}
func (p Posts) Swap(i, j int) { p[i], p[j] = p[j], p[i] }
