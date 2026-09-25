package torrentmeta

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Metadata is the native identity carried by a .torrent file. Name is
// info.name (preferably info.name.utf-8). Files contains the native paths from
// info.files for multi-file torrents, preferring path.utf-8. Reading this data
// downloads only the small .torrent metainfo object, never media payload.
type Metadata struct {
	Name     string
	Files    []string
	InfoHash string
}

func Parse(data []byte) (Metadata, error) {
	if len(data) == 0 || data[0] != 'd' {
		return Metadata{}, errors.New("torrent metainfo root is not a dictionary")
	}
	i := 1
	var infoStart, infoEnd int
	for i < len(data) && data[i] != 'e' {
		key, next, err := readString(data, i)
		if err != nil {
			return Metadata{}, fmt.Errorf("decode torrent key: %w", err)
		}
		i = next
		start := i
		end, err := skipValue(data, i)
		if err != nil {
			return Metadata{}, fmt.Errorf("decode torrent value %q: %w", key, err)
		}
		if string(key) == "info" {
			infoStart, infoEnd = start, end
		}
		i = end
	}
	if infoStart == 0 || infoEnd <= infoStart {
		return Metadata{}, errors.New("torrent metainfo has no info dictionary")
	}
	name, files, err := infoIdentity(data[infoStart:infoEnd])
	if err != nil {
		return Metadata{}, err
	}
	sum := sha1.Sum(data[infoStart:infoEnd])
	return Metadata{Name: name, Files: files, InfoHash: hex.EncodeToString(sum[:])}, nil
}

func infoIdentity(info []byte) (string, []string, error) {
	if len(info) == 0 || info[0] != 'd' {
		return "", nil, errors.New("torrent info is not a dictionary")
	}
	i := 1
	var plain, utf8 []byte
	var files []string
	for i < len(info) && info[i] != 'e' {
		key, next, err := readString(info, i)
		if err != nil {
			return "", nil, fmt.Errorf("decode info key: %w", err)
		}
		i = next
		switch string(key) {
		case "name", "name.utf-8":
			value, end, err := readString(info, i)
			if err != nil {
				return "", nil, fmt.Errorf("decode info %s: %w", key, err)
			}
			if string(key) == "name.utf-8" {
				utf8 = append([]byte(nil), value...)
			} else {
				plain = append([]byte(nil), value...)
			}
			i = end
		case "files":
			values, end, err := readFileList(info, i)
			if err != nil {
				return "", nil, fmt.Errorf("decode info files: %w", err)
			}
			files = values
			i = end
		default:
			end, err := skipValue(info, i)
			if err != nil {
				return "", nil, err
			}
			i = end
		}
	}
	name := plain
	if len(utf8) > 0 {
		name = utf8
	}
	if len(name) == 0 {
		return "", nil, errors.New("torrent info has no name")
	}
	return string(name), files, nil
}

func readFileList(data []byte, i int) ([]string, int, error) {
	if i >= len(data) || data[i] != 'l' {
		return nil, 0, errors.New("files is not a list")
	}
	i++
	var out []string
	for i < len(data) && data[i] != 'e' {
		if data[i] != 'd' {
			return nil, 0, errors.New("file entry is not a dictionary")
		}
		i++
		var plain, utf8 []string
		for i < len(data) && data[i] != 'e' {
			key, next, err := readString(data, i)
			if err != nil {
				return nil, 0, err
			}
			i = next
			switch string(key) {
			case "path", "path.utf-8":
				parts, end, err := readStringList(data, i)
				if err != nil {
					return nil, 0, err
				}
				if string(key) == "path.utf-8" {
					utf8 = parts
				} else {
					plain = parts
				}
				i = end
			default:
				end, err := skipValue(data, i)
				if err != nil {
					return nil, 0, err
				}
				i = end
			}
		}
		if i >= len(data) || data[i] != 'e' {
			return nil, 0, errors.New("unterminated file dictionary")
		}
		i++
		parts := plain
		if len(utf8) > 0 {
			parts = utf8
		}
		if len(parts) > 0 {
			out = append(out, strings.Join(parts, "/"))
		}
	}
	if i >= len(data) || data[i] != 'e' {
		return nil, 0, errors.New("unterminated files list")
	}
	return out, i + 1, nil
}

func readStringList(data []byte, i int) ([]string, int, error) {
	if i >= len(data) || data[i] != 'l' {
		return nil, 0, errors.New("path is not a list")
	}
	i++
	var out []string
	for i < len(data) && data[i] != 'e' {
		value, next, err := readString(data, i)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, string(value))
		i = next
	}
	if i >= len(data) || data[i] != 'e' {
		return nil, 0, errors.New("unterminated path list")
	}
	return out, i + 1, nil
}

func skipValue(data []byte, i int) (int, error) {
	if i >= len(data) {
		return 0, errors.New("unexpected end of bencode")
	}
	switch data[i] {
	case 'i':
		end := i + 1
		for end < len(data) && data[end] != 'e' {
			end++
		}
		if end >= len(data) {
			return 0, errors.New("unterminated integer")
		}
		if end == i+1 {
			return 0, errors.New("empty integer")
		}
		return end + 1, nil
	case 'l', 'd':
		j := i + 1
		for {
			if j >= len(data) {
				return 0, errors.New("unterminated container")
			}
			if data[j] == 'e' {
				return j + 1, nil
			}
			if data[i] == 'd' {
				_, next, err := readString(data, j)
				if err != nil {
					return 0, err
				}
				j = next
			}
			next, err := skipValue(data, j)
			if err != nil {
				return 0, err
			}
			j = next
		}
	default:
		_, end, err := readString(data, i)
		return end, err
	}
}

func readString(data []byte, i int) ([]byte, int, error) {
	if i >= len(data) || data[i] < '0' || data[i] > '9' {
		return nil, 0, errors.New("expected byte string")
	}
	colon := i
	for colon < len(data) && data[colon] >= '0' && data[colon] <= '9' {
		colon++
	}
	if colon >= len(data) || data[colon] != ':' {
		return nil, 0, errors.New("invalid byte string length")
	}
	n, err := strconv.Atoi(string(data[i:colon]))
	if err != nil || n < 0 {
		return nil, 0, errors.New("invalid byte string length")
	}
	start := colon + 1
	end := start + n
	if end < start || end > len(data) {
		return nil, 0, errors.New("byte string exceeds input")
	}
	return data[start:end], end, nil
}
