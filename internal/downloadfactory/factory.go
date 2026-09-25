package downloadfactory

import (
	"fmt"
	"sort"

	"aninode/internal/configstore"
	"aninode/internal/download"
	"aninode/internal/download/aria2"
	"aninode/internal/download/qbittorrent"
	"aninode/internal/download/transmission"
)

type CredentialLookup func(clientID string) (string, error)

func FromConfig(config configstore.Client, credential string) (download.Backend, error) {
	mappings := make([]download.PathMapping, 0, len(config.PathMappings))
	for _, mapping := range config.PathMappings {
		mappings = append(mappings, download.PathMapping{Remote: mapping.Remote, Local: mapping.Local})
	}
	switch config.Type {
	case "qbittorrent":
		return qbittorrent.New(qbittorrent.Options{Name: config.ID, BaseURL: config.URL, Username: config.Username, Password: credential, PathMappings: mappings})
	case "transmission":
		return transmission.New(transmission.Options{Name: config.ID, BaseURL: config.URL, Username: config.Username, Password: credential, PathMappings: mappings})
	case "aria2":
		return aria2.New(aria2.Options{Name: config.ID, Endpoint: config.URL, Secret: credential, PathMappings: mappings})
	default:
		return nil, fmt.Errorf("unsupported download client type %q", config.Type)
	}
}

func EnabledFromConfig(configs map[string]configstore.Client, lookup CredentialLookup) ([]download.Backend, error) {
	ids := make([]string, 0, len(configs))
	for id := range configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var clients []download.Backend
	for _, id := range ids {
		if !configs[id].Enabled {
			continue
		}
		credential, err := lookup(id)
		if err != nil {
			return nil, fmt.Errorf("resolve client %q credential: %w", id, err)
		}
		client, err := FromConfig(configs[id], credential)
		if err != nil {
			return nil, err
		}
		clients = append(clients, client)
	}
	return clients, nil
}
