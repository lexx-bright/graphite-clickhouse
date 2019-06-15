package prometheus

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"strings"
	template_text "text/template"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/rules"
	"github.com/prometheus/prometheus/scrape"
	"github.com/prometheus/prometheus/template"
	"github.com/prometheus/prometheus/web"

	"github.com/lomik/graphite-clickhouse/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/route"
	"github.com/prometheus/common/server"
	v1 "github.com/prometheus/prometheus/web/api/v1"
	"github.com/prometheus/prometheus/web/ui"
)

type Handler struct {
	config      *config.Config
	apiV1       *v1.API
	apiV1Router *route.Router
	web         *web.Handler
	queryEngine *promql.Engine
}

func NewHandler(config *config.Config) *Handler {
	h := &Handler{
		config:      config,
		queryEngine: promql.NewEngine(promql.EngineOpts{MaxConcurrent: 100, MaxSamples: 1000000, Timeout: time.Minute}),
	}

	apiV1 := v1.NewAPI(
		h.queryEngine, // qe *promql.Engine,
		h,             // q storage.Queryable,
		nil,           // tr targetRetriever,
		nil,           // ar alertmanagerRetriever,
		nil,           // configFunc func() config.Config,
		nil,           // flagsMap map[string]string,
		func(f http.HandlerFunc) http.HandlerFunc { return f }, // readyFunc func(http.HandlerFunc) http.HandlerFunc,
		nil,   // db func() TSDBAdmin,
		false, // enableAdmin bool,
		nil,   // logger log.Logger,
		nil,   // rr rulesRetriever,
		0,     // remoteReadSampleLimit int,
		0,     // remoteReadConcurrencyLimit int,
		nil,   // CORSOrigin *regexp.Regexp,
	)

	apiV1Router := route.New()

	apiV1.Register(apiV1Router)

	h.apiV1 = apiV1
	h.apiV1Router = apiV1Router
	h.web = &web.Handler{}

	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/read") {
		h.read(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/api/v1") {
		http.StripPrefix("/api/v1", h.apiV1Router).ServeHTTP(w, r)
		return
	}

	if r.URL.Path == "/graph" {
		h.graph(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/static/") {
		fs := server.StaticFileServer(ui.Assets)
		fs.ServeHTTP(w, r)
		return
	}

	http.Redirect(w, r, path.Join(h.config.Prometheus.ExternalURL.Path, "/graph"), http.StatusFound)
}

func (h *Handler) graph(w http.ResponseWriter, r *http.Request) {
	h.executeTemplate(w, "graph.html", nil)
}

func (h *Handler) getTemplate(name string) (string, error) {
	var tmpl string

	appendf := func(name string) error {
		f, err := ui.Assets.Open(path.Join("/templates", name))
		if err != nil {
			return err
		}
		defer f.Close()
		b, err := ioutil.ReadAll(f)
		if err != nil {
			return err
		}
		tmpl += string(b)
		return nil
	}

	err := appendf("_base.html")
	if err != nil {
		return "", errors.Wrap(err, "error reading base template")
	}
	err = appendf(name)
	if err != nil {
		return "", errors.Wrapf(err, "error reading page template %s", name)
	}

	return tmpl, nil
}

func (h *Handler) executeTemplate(w http.ResponseWriter, name string, data interface{}) {
	text, err := h.getTemplate(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	tmpl := template.NewTemplateExpander(
		context.Background(),
		text,
		name,
		data,
		model.Time(time.Now().UnixNano()/1000000),
		template.QueryFunc(rules.EngineQueryFunc(h.queryEngine, nil)),
		h.config.Prometheus.ExternalURL,
	)
	tmpl.Funcs(tmplFuncs(h, h.config.Prometheus.ExternalURL.Path+"/consoles/index.html"))

	result, err := tmpl.ExpandHTML(nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	io.WriteString(w, result)
}

func tmplFuncs(h *Handler, consolesPath string) template_text.FuncMap {
	return template_text.FuncMap{
		"since": func(t time.Time) time.Duration {
			return time.Since(t) / time.Millisecond * time.Millisecond
		},
		"consolesPath": func() string { return consolesPath },
		"pathPrefix":   func() string { return h.config.Prometheus.ExternalURL.Path },
		"pageTitle":    func() string { return h.config.Prometheus.PageTitle },
		"buildVersion": func() string { return fmt.Sprint(time.Now().Unix()) },
		"globalURL": func(u *url.URL) *url.URL {
			return u
		},
		"numHealthy": func(pool []*scrape.Target) int {
			alive := len(pool)
			for _, p := range pool {
				if p.Health() != scrape.HealthGood {
					alive--
				}
			}

			return alive
		},
		"targetHealthToClass": func(th scrape.TargetHealth) string {
			switch th {
			case scrape.HealthUnknown:
				return "warning"
			case scrape.HealthGood:
				return "success"
			default:
				return "danger"
			}
		},
		"ruleHealthToClass": func(rh rules.RuleHealth) string {
			switch rh {
			case rules.HealthUnknown:
				return "warning"
			case rules.HealthGood:
				return "success"
			default:
				return "danger"
			}
		},
		"alertStateToClass": func(as rules.AlertState) string {
			switch as {
			case rules.StateInactive:
				return "success"
			case rules.StatePending:
				return "warning"
			case rules.StateFiring:
				return "danger"
			default:
				panic("unknown alert state")
			}
		},
	}
}
