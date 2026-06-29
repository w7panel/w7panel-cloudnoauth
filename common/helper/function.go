package helper

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

func BuildSign(data map[string]any, token string) string {
	return MD5String(BuildPHPQuery(data) + token)
}

func ParsePHPFormBody(body []byte) (map[string]any, error) {
	data := map[string]any{}
	if len(body) == 0 {
		return data, nil
	}

	for _, pair := range strings.Split(string(body), "&") {
		if pair == "" {
			continue
		}

		key, value, _ := strings.Cut(pair, "=")
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			return nil, err
		}
		decodedValue, err := url.QueryUnescape(value)
		if err != nil {
			return nil, err
		}

		insertPHPFormValue(data, parsePHPFormKey(decodedKey), decodedValue)
	}

	return data, nil
}

func BuildPHPQuery(data map[string]any) string {
	return buildPHPQuery(data, false)
}

func EncodePHPQuery(data map[string]any) string {
	return buildPHPQuery(data, true)
}

func buildPHPQuery(data map[string]any, includeSign bool) string {
	keys := make([]string, 0, len(data))
	for key := range data {
		if includeSign || key != "sign" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = appendPHPQueryValue(parts, key, data[key])
	}
	return strings.Join(parts, "&")
}

func MD5String(value string) string {
	sum := md5.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}

func RandomString(length int) (string, error) {
	const letters = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	buffer := make([]byte, length)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}

	for index := range buffer {
		buffer[index] = letters[int(buffer[index])%len(letters)]
	}
	return string(buffer), nil
}

func appendPHPQueryValue(parts []string, key string, value any) []string {
	switch typedValue := value.(type) {
	case nil:
		return append(parts, url.QueryEscape(key)+"=")
	case map[string]any:
		keys := make([]string, 0, len(typedValue))
		for childKey := range typedValue {
			keys = append(keys, childKey)
		}
		sort.Strings(keys)
		for _, childKey := range keys {
			parts = appendPHPQueryValue(parts, key+"["+childKey+"]", typedValue[childKey])
		}
		return parts
	case []any:
		for index, childValue := range typedValue {
			parts = appendPHPQueryValue(parts, fmt.Sprintf("%s[%d]", key, index), childValue)
		}
		return parts
	case json.Number:
		return append(parts, url.QueryEscape(key)+"="+url.QueryEscape(typedValue.String()))
	case bool:
		if typedValue {
			return append(parts, url.QueryEscape(key)+"=1")
		}
		return append(parts, url.QueryEscape(key)+"=0")
	default:
		return append(parts, url.QueryEscape(key)+"="+url.QueryEscape(fmt.Sprintf("%v", typedValue)))
	}
}

func parsePHPFormKey(key string) []string {
	if key == "" {
		return []string{""}
	}

	parts := make([]string, 0, 3)
	baseEnd := strings.IndexByte(key, '[')
	if baseEnd < 0 {
		return []string{key}
	}
	if baseEnd > 0 {
		parts = append(parts, key[:baseEnd])
	}

	rest := key[baseEnd:]
	for len(rest) > 0 {
		if rest[0] != '[' {
			parts = append(parts, rest)
			break
		}

		end := strings.IndexByte(rest, ']')
		if end < 0 {
			parts = append(parts, rest)
			break
		}

		parts = append(parts, rest[1:end])
		rest = rest[end+1:]
	}

	if len(parts) == 0 {
		return []string{key}
	}
	return parts
}

func insertPHPFormValue(data map[string]any, parts []string, value string) {
	if len(parts) == 0 {
		return
	}

	key := parts[0]
	if key == "" {
		key = nextPHPArrayIndex(data)
	}

	if len(parts) == 1 {
		data[key] = value
		return
	}

	child, ok := data[key].(map[string]any)
	if !ok {
		child = map[string]any{}
		data[key] = child
	}
	insertPHPFormValue(child, parts[1:], value)
}

func nextPHPArrayIndex(data map[string]any) string {
	next := 0
	for key := range data {
		var index int
		if _, err := fmt.Sscanf(key, "%d", &index); err == nil && index >= next {
			next = index + 1
		}
	}
	return fmt.Sprintf("%d", next)
}
