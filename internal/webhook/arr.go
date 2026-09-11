package webhook

import "errors"

type arrFile struct {
	Path string `json:"path"`
}

type sonarrPayload struct {
	EventType   string   `json:"eventType"`
	EpisodeFile *arrFile `json:"episodeFile"`
	// On Import Complete: the same event type, one post per release.
	EpisodeFiles []*arrFile `json:"episodeFiles"`
}

func fromSonarr(p sonarrPayload) (Request, error) {
	return imported(p.EventType, append([]*arrFile{p.EpisodeFile}, p.EpisodeFiles...))
}

type radarrPayload struct {
	EventType string   `json:"eventType"`
	MovieFile *arrFile `json:"movieFile"`
}

func fromRadarr(p radarrPayload) (Request, error) {
	return imported(p.EventType, []*arrFile{p.MovieFile})
}

// An upgrade's deletedFiles are the recycled originals, so they are not read.
func imported(eventType string, files []*arrFile) (Request, error) {
	if eventType != "Download" {
		return Request{}, nil
	}
	var req Request
	for _, f := range files {
		if f != nil && f.Path != "" {
			req.Paths = append(req.Paths, f.Path)
		}
	}
	if len(req.Paths) == 0 {
		return Request{}, errors.New("import names no file")
	}
	return req, nil
}
