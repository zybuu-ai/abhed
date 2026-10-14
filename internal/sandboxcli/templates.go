package sandboxcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
)

const templatesPath = "/v1/sandbox-templates"

func (r *runner) template(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(r.Err, "usage: abhed sandbox template list|get|create|rm [flags]")
		return usageError{errUsagePrinted}
	}
	switch args[0] {
	case "list", "ls":
		return r.templateList(args[1:])
	case "get":
		return r.templateGet(args[1:])
	case "create":
		return r.templateCreate(args[1:])
	case "rm", "delete":
		return r.templateRm(args[1:])
	}
	return usageError{fmt.Errorf("no template command %q; use list, get, create or rm", args[0])}
}

// oneName checks the arguments are exactly one template name.
func oneName(pos []string, what string) (string, error) {
	if len(pos) != 1 {
		return "", usageError{fmt.Errorf("%s takes one template name", what)}
	}
	if !controlplane.ValidTemplateName(pos[0]) {
		return "", usageError{fmt.Errorf("%q is not a template name (letters, digits, '.', '_' or '-')", pos[0])}
	}
	return pos[0], nil
}

const templateHeader = "NAME\tDRIVER\tLABELS\tSETTINGS\tDESCRIPTION\n"

func templateRow(tw *tabwriter.Writer, t controlplane.Template) {
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", clean(t.Name, 0), clean(orDash(t.Driver), 0), joinMap(t.Labels), joinMap(t.Settings),
		clean(orDash(t.Description), 60))
}

type templateList struct {
	Items []controlplane.Template `json:"items"`
	Next  string                  `json:"next,omitempty"`
}

func (r *runner) templateList(args []string) error {
	fs := r.flags("template list", "[-l SELECTOR]")
	sel := fs.String("l", "", "a label selector, such as team=a")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError{errors.New("template list takes no arguments")}
	}
	if _, err := controlplane.ParseSelector(*sel); err != nil {
		return usageError{err}
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	var out templateList
	after := ""
	for {
		q := url.Values{}
		if *sel != "" {
			q.Set("selector", *sel)
		}
		if after != "" {
			q.Set("after", after)
		}
		var page templateList
		if _, err := r.cl.json(r.Ctx, call{method: http.MethodGet, path: templatesPath + "?" + q.Encode()}, &page); err != nil {
			return err
		}
		out.Items = append(out.Items, page.Items...)
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	if out.Items == nil {
		out.Items = []controlplane.Template{}
	}
	return r.emit(out, func(tw *tabwriter.Writer) {
		fmt.Fprint(tw, templateHeader)
		for _, t := range out.Items {
			templateRow(tw, t)
		}
	})
}

func (r *runner) templateGet(args []string) error {
	fs := r.flags("template get", "NAME")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	name, err := oneName(pos, "template get")
	if err != nil {
		return err
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	var t controlplane.Template
	if _, err := r.cl.json(r.Ctx, call{method: http.MethodGet, path: templatesPath + "/" + name}, &t); err != nil {
		return err
	}
	return r.emit(t, func(tw *tabwriter.Writer) {
		row := func(k, v string) { fmt.Fprintf(tw, "%s:\t%s\n", k, clean(v, 0)) }
		row("Name", t.Name)
		row("Description", orDash(t.Description))
		row("Driver", orDash(t.Driver))
		row("Labels", joinMap(t.Labels))
		row("Settings", joinMap(t.Settings))
		row("Owner", t.Owner)
		row("Version", strconv.FormatInt(t.ResourceVersion, 10))
		row("Created", t.CreatedAt.Format(time.RFC3339))
	})
}

func (r *runner) templateCreate(args []string) error {
	fs := r.flags("template create", "NAME [flags]")
	desc := fs.String("description", "", "what it is for")
	drv := fs.String("driver", "", "the driver every sandbox from it uses")
	var sf settingsFlags
	fs.Var(&sf.labels, "l", "a label key=value (repeatable); sandboxes made from it get it too")
	fs.Var(&sf.set, "set", "a setting key=value (repeatable); a create may tighten it, never loosen it")
	fs.StringVar(&sf.minTier, "min-tier", "", "the weakest isolation acceptable: process, fence, container, vm or microvm")
	fs.StringVar(&sf.cpus, "cpus", "", "CPUs, such as 0.5 or 2")
	fs.IntVar(&sf.memoryMB, "memory-mb", 0, "memory in MiB")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	name, err := oneName(pos, "template create")
	if err != nil {
		return err
	}
	labels, err := pairs("l", sf.labels)
	if err != nil {
		return err
	}
	settings, err := sf.values()
	if err != nil {
		return err
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"name": name, "description": *desc, "driver": *drv, "labels": labels, "settings": settings})
	var t controlplane.Template
	if _, err := r.cl.json(r.Ctx, call{method: http.MethodPost, path: templatesPath, body: body, mutating: true}, &t); err != nil {
		return err
	}
	return r.emit(t, func(tw *tabwriter.Writer) {
		fmt.Fprint(tw, templateHeader)
		templateRow(tw, t)
	})
}

func (r *runner) templateRm(args []string) error {
	fs := r.flags("template rm", "NAME [-y]")
	yes := fs.Bool("y", false, "do not ask first")
	ifMatch := fs.String("if-match", "", "remove it only at this version")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	name, err := oneName(pos, "template rm")
	if err != nil {
		return err
	}
	if !*yes {
		if err := r.confirm("template rm", "Remove template "+name+"? Sandboxes made from it keep what it gave them."); err != nil {
			return err
		}
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	c := call{method: http.MethodDelete, path: templatesPath + "/" + name, mutating: true}
	if e := etagOf(*ifMatch); e != "" {
		c.header = http.Header{"If-Match": {e}}
	}
	var res map[string]string
	if _, err := r.cl.json(r.Ctx, c, &res); err != nil {
		return err
	}
	return r.emit(res, func(tw *tabwriter.Writer) { fmt.Fprintf(tw, "removed template %s\n", name) })
}
