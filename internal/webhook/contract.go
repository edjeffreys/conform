// Package webhook turns posts from other services into paths worth judging.
// README.md defines the contract a mapper for a new service must meet.
package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
)

type Request struct {
	Paths []string `json:"paths"`
}

// Checked centrally, so no mapper can hand the server a path that would be
// resolved against conform's own working directory.
func (r Request) validate() error {
	for _, p := range r.Paths {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("path %q is not absolute", p)
		}
	}
	return nil
}

type Mapper struct {
	Name   string
	decode func(body []byte) (Request, error)
}

func Map[Payload any](name string, mapping func(Payload) (Request, error)) Mapper {
	return Mapper{
		Name: name,
		decode: func(body []byte) (Request, error) {
			var p Payload
			if err := json.Unmarshal(body, &p); err != nil {
				return Request{}, fmt.Errorf("not a %s payload: %w", name, err)
			}
			req, err := mapping(p)
			if err != nil {
				return Request{}, err
			}
			return req, req.validate()
		},
	}
}

func (m Mapper) Route() string {
	if m.Name == "" {
		return "/webhook"
	}
	return "/webhook/" + m.Name
}

var direct = Map("", func(r Request) (Request, error) {
	if len(r.Paths) == 0 {
		return Request{}, errors.New("request names no paths")
	}
	return r, nil
})
