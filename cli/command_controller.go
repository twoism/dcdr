package cli

import (
	"encoding/json"
	"os"

	"errors"

	"io/ioutil"

	"path"

	"github.com/tucnak/climax"
	"github.com/vsco/dcdr/cli/api"
	"github.com/vsco/dcdr/cli/models"
	"github.com/vsco/dcdr/cli/printer"
	"github.com/vsco/dcdr/cli/ui"
	"github.com/vsco/dcdr/client"
	"github.com/vsco/dcdr/config"
	"github.com/vsco/dcdr/server"
	"github.com/vsco/dcdr/server/middleware"
	"github.com/zenazn/goji"
)

const FilePerms = 0755

var (
	InvalidFeatureTypeError = errors.New("invalid -value format. use -value=[0.0-1.0] or [true|false]")
	InvalidRangeError       = errors.New("invalid -value for percentile. use -value=[0.0-1.0]")
	NameRequiredError       = errors.New("-name is required")
)

type Controller struct {
	Config *config.Config
	Client api.ClientIFace
}

func NewController(cfg *config.Config, kv api.ClientIFace) (cc *Controller) {
	cc = &Controller{
		Config: cfg,
		Client: kv,
	}

	return
}

func (cc *Controller) List(ctx climax.Context) int {
	pf, _ := ctx.Get("prefix")
	scope, _ := ctx.Get("scope")

	if pf != "" && scope == "" {
		scope = models.DefaultScope
	}

	features, err := cc.Client.List(pf, scope)

	if err != nil {
		printer.SayErr("%v", err)
		return 1
	}

	if len(features) == 0 {
		printer.Say("no feature flags found in namespace: %s",
			cc.Client.Namespace())
		return 1
	}

	ui.New().DrawFeatures(features)

	return 0
}

func (cc *Controller) Set(ctx climax.Context) int {
	ft, err := cc.ParseContext(ctx)

	if err != nil {
		printer.SayErr("parse error: %v", err)
		return 1
	}

	err = cc.Client.Set(ft)

	if err != nil {
		printer.SayErr("set error: %v", err)
		return 1
	}

	printer.Say("set flag '%s'", ft.ScopedKey())

	return cc.CommitFeatures(ft, false)
}

func (cc *Controller) Delete(ctx climax.Context) int {
	name, _ := ctx.Get("name")
	scope, _ := ctx.Get("scope")

	if name == "" {
		printer.Say("name is required")
		return 1
	}

	if scope == "" {
		scope = models.DefaultScope
	}

	err := cc.Client.Delete(name, scope)

	if err != nil {
		printer.SayErr("%v", err)
		return 1
	}

	printer.Say("deleted flag %s/%s/%s",
		cc.Config.Namespace, scope, name)

	ft := &models.Feature{
		Key:       name,
		Scope:     scope,
		UpdatedBy: cc.Config.Username,
	}

	return cc.CommitFeatures(ft, true)
}

func (cc *Controller) CommitFeatures(ft *models.Feature, deleted bool) int {
	if cc.Config.GitEnabled() {
		printer.Say("committing changes")
		err := cc.Client.Commit(ft, deleted)

		if err != nil {
			printer.SayErr("%v", err)
			return 1
		}

		sha, err := cc.Client.UpdateCurrentSha()
		printer.Say("set info/current_sha: %s", sha)

		if err != nil {
			printer.SayErr("%v", err)
			return 1
		}

		if cc.Config.PushEnabled() {
			printer.Say("pushing commit to origin")
			err = cc.Client.Push()

			if err != nil {
				printer.SayErr("%v", err)
				return 1
			}
		}

	}

	return 0
}

func (cc *Controller) Init(ctx climax.Context) int {
	if _, err := os.Stat(config.ConfigPath()); os.IsNotExist(err) {
		err = os.MkdirAll(path.Dir(config.ConfigPath()), FilePerms)

		printer.Say("creating %s", path.Dir(config.ConfigPath()))

		if err != nil {
			printer.SayErr("could not create config directory: %v", err)
			return 1
		}

		err = ioutil.WriteFile(config.ConfigPath(), config.ExampleConfig, FilePerms)
		printer.Say("%s not found. creating example config", config.ConfigPath())

		if err != nil {
			printer.SayErr("could not write config.hcl %v", err)
			return 1
		}
	}

	if !cc.Config.GitEnabled() {
		printer.Say("no repository configured. skipping")
		return 0
	}

	create := ctx.Is("create")

	err := cc.Client.InitRepo(create)

	if err != nil {
		printer.SayErr("%v", err)
		return 1
	}

	if create {
		printer.Say("initialized new repo in %s and pushed to %s",
			cc.Config.Git.RepoPath, cc.Config.Git.RepoURL)
	} else {
		printer.Say("cloned %s into %s",
			cc.Config.Git.RepoURL, cc.Config.Git.RepoPath)
	}

	return 0
}

func (cc *Controller) Import(ctx climax.Context) int {
	bts, err := ioutil.ReadAll(os.Stdin)

	if err != nil {
		printer.SayErr("%v", err)
		return 1
	}

	var kvs map[string]interface{}
	err = json.Unmarshal(bts, &kvs)

	if err != nil {
		printer.SayErr("%v", err)
		return 1
	}

	scope, _ := ctx.Get("scope")

	if scope == "" {
		scope = models.DefaultScope
	}

	for k, v := range kvs {
		f := models.NewFeature(k, v, "", "", scope, cc.Config.Namespace)
		err = cc.Client.Set(f)

		if err != nil {
			printer.SayErr("%v", err)
			return 1
		}

		printer.Say("set %s to %+v", k, v)
	}

	return 1
}

func (cc *Controller) Info(ctx climax.Context) int {

	ui.New().DrawConfig(cc.Config)

	return 0
}

func (cc *Controller) Serve(ctx climax.Context) int {
	c, err := client.New(cc.Config).Watch()

	if err != nil {
		panic(err)
	}

	s := server.New(cc.Config, goji.DefaultMux, c)
	s.Use(middleware.HTTPCachingHandler(c))
	s.Serve()

	return 0
}

func (cc *Controller) Watch(ctx climax.Context) int {

	printer.Log("watching namespace: %s", cc.Config.Namespace)

	cc.Client.Watch()

	return 0
}

func (cc *Controller) ParseContext(ctx climax.Context) (*models.Feature, error) {
	name, _ := ctx.Get("name")
	val, _ := ctx.Get("value")
	cmt, _ := ctx.Get("comment")
	scp, _ := ctx.Get("scope")

	if name == "" {
		return nil, NameRequiredError
	}

	var v interface{}
	var ft models.FeatureType

	if val != "" {
		v, ft = models.ParseValueAndFeatureType(val)

		if ft == models.Invalid {
			return nil, InvalidFeatureTypeError
		}

		if ft == models.Percentile {
			if v.(float64) > 1.0 || v.(float64) < 0 {
				return nil, InvalidRangeError
			}
		}
	}

	f := models.NewFeature(name, v, cmt, cc.Config.Username, scp, cc.Config.Namespace)
	f.FeatureType = ft

	return f, nil
}
