package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/xattribution/gohelm/cdp"
)

func sortStrings(s []string) { sort.Strings(s) }

// world is gohelm's isolated world in a frame, made when missing.
func (p *Page) world(ctx context.Context, frame string) (int, error) {
	p.mu.Lock()
	if frame == "" {
		frame = p.mainFrame
	}
	id, ok := p.worlds[frame]
	p.mu.Unlock()
	if ok {
		return id, nil
	}
	var r struct {
		ID int `json:"executionContextId"`
	}
	if err := cdp.Do(ctx, p.t, "Page.createIsolatedWorld", map[string]any{"frameId": frame, "worldName": worldName}, &r); err != nil {
		return 0, err
	}
	// A world made now missed the document's start: run the script in it.
	p.t.Call(ctx, "Runtime.evaluate", map[string]any{"expression": worldScript, "contextId": r.ID})
	p.mu.Lock()
	p.worlds[frame] = r.ID
	p.mu.Unlock()
	return r.ID, nil
}

type remoteResult struct {
	Result struct {
		Type     string          `json:"type"`
		Value    json.RawMessage `json:"value"`
		ObjectID string          `json:"objectId"`
	} `json:"result"`
	Exception *struct {
		Text      string `json:"text"`
		Exception struct {
			Description string `json:"description"`
		} `json:"exception"`
	} `json:"exceptionDetails"`
}

func (r remoteResult) err() error {
	if r.Exception == nil {
		return nil
	}
	d := r.Exception.Exception.Description
	if d == "" {
		d = r.Exception.Text
	}
	return fmt.Errorf("browser: page script failed: %s", d)
}

// evalWorld evaluates expr in gohelm's world of a frame and decodes the
// value into out.
func (p *Page) evalWorld(ctx context.Context, frame, expr string, out any) error {
	if p.Dialog() != nil {
		return ErrDialog // scripts wait for the dialog
	}
	id, err := p.world(ctx, frame)
	if err != nil {
		return err
	}
	var r remoteResult
	if err := cdp.Do(ctx, p.t, "Runtime.evaluate", map[string]any{"expression": expr, "contextId": id, "returnByValue": true}, &r); err != nil {
		var ce *cdp.Error
		if errors.As(err, &ce) { // the world went with a navigation: make it again once
			p.mu.Lock()
			delete(p.worlds, frame)
			p.mu.Unlock()
			if id, err = p.world(ctx, frame); err != nil {
				return err
			}
			if err := cdp.Do(ctx, p.t, "Runtime.evaluate", map[string]any{"expression": expr, "contextId": id, "returnByValue": true}, &r); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	if err := r.err(); err != nil {
		return err
	}
	if out != nil && len(r.Result.Value) > 0 {
		return json.Unmarshal(r.Result.Value, out)
	}
	return nil
}

// callOn runs fn (a function declaration) with the node as this, in
// gohelm's world of the node's frame, and decodes its returned value.
// Extra arguments are JSON values.
func (p *Page) callOn(ctx context.Context, t refTarget, fn string, out any, args ...any) error {
	if p.Dialog() != nil {
		return ErrDialog
	}
	id, err := p.world(ctx, t.frame)
	if err != nil {
		return err
	}
	var obj struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	if err := cdp.Do(ctx, p.t, "DOM.resolveNode", map[string]any{"backendNodeId": t.backend, "executionContextId": id, "objectGroup": worldName}, &obj); err != nil {
		return errGone
	}
	defer p.t.Call(context.WithoutCancel(ctx), "Runtime.releaseObject", map[string]any{"objectId": obj.Object.ObjectID})
	var cargs []map[string]any
	for _, a := range args {
		if o, ok := a.(objectArg); ok {
			cargs = append(cargs, map[string]any{"objectId": string(o)})
			continue
		}
		cargs = append(cargs, map[string]any{"value": a})
	}
	var r remoteResult
	params := map[string]any{"objectId": obj.Object.ObjectID, "functionDeclaration": fn, "returnByValue": true}
	if len(cargs) > 0 {
		params["arguments"] = cargs
	}
	if err := cdp.Do(ctx, p.t, "Runtime.callFunctionOn", params, &r); err != nil {
		return err
	}
	if err := r.err(); err != nil {
		return err
	}
	if out != nil && len(r.Result.Value) > 0 {
		return json.Unmarshal(r.Result.Value, out)
	}
	return nil
}

// objectArg passes a remote object to callOn.
type objectArg string

// resolveIn gives a node's remote object in gohelm's world of frame.
func (p *Page) resolveIn(ctx context.Context, frame string, backend int) (objectArg, error) {
	id, err := p.world(ctx, frame)
	if err != nil {
		return "", err
	}
	var obj struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	if err := cdp.Do(ctx, p.t, "DOM.resolveNode", map[string]any{"backendNodeId": backend, "executionContextId": id, "objectGroup": worldName}, &obj); err != nil {
		return "", err
	}
	return objectArg(obj.Object.ObjectID), nil
}

// errGone is a ref whose element is no longer on the page.
var errGone = errors.New("that element is gone from the page; take a new snapshot")
