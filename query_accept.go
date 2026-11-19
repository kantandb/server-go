package main

import (
	"mime"
	"strconv"
	"strings"
)

func queryResponseType(headers []string) string {
	if strings.TrimSpace(strings.Join(headers, "")) == "" {
		return queryJSON
	}

	json := queryQuality(headers, queryJSON)
	ndjson := queryQuality(headers, queryNDJSON)
	switch {
	case ndjson > json:
		return queryNDJSON
	case json > 0:
		return queryJSON
	default:
		return ""
	}
}

func queryQuality(headers []string, offered string) float64 {
	best, specificity := 0.0, 0
	for _, header := range headers {
		for item := range strings.SplitSeq(header, ",") {
			mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(item))
			if err != nil {
				continue
			}

			rank := 0
			switch mediaType {
			case offered:
				rank = 3
			case "application/*":
				rank = 2
			case "*/*":
				rank = 1
			default:
				continue
			}

			quality := 1.0
			if q, ok := params["q"]; ok {
				quality, err = strconv.ParseFloat(q, 64)
				if err != nil || !(quality >= 0 && quality <= 1) {
					continue
				}
				delete(params, "q")
			}

			rank *= 2
			if len(params) != 0 {
				if len(params) != 1 || offered != queryJSON || !strings.EqualFold(params["charset"], "utf-8") {
					continue
				}
				rank++
			}

			// An explicit exclusion overrides a less specific wildcard.
			if rank > specificity || rank == specificity && quality > best {
				best, specificity = quality, rank
			}
		}
	}

	return best
}
