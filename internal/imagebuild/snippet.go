// SPDX-License-Identifier: AGPL-3.0-only

package imagebuild

import (
	"strings"
	"unicode/utf8"
)

// Limits of the custom nginx snippet.
const (
	MaxSnippetBytes = 8 << 10
	MaxSnippetLines = 200
)

// deniedDirectives never appear in a snippet: they load code, include other
// files, redirect logs or pid files, reference key material or break the
// image's process model.
var deniedDirectives = map[string]bool{
	"load_module": true, "include": true, "user": true, "pid": true, "daemon": true,
	"env": true, "working_directory": true, "error_log": true, "access_log": true,
	"ssl_certificate_key": true, "ssl_password_file": true, "auth_basic_user_file": true,
	"master_process": true, "worker_rlimit_core": true, "lock_file": true,
	"proxy_ssl_certificate_key": true, "uwsgi_pass": true, "scgi_pass": true,
	// File and request routing directives can escape the generated document
	// root or expose files and credentials inside the build image.
	"root": true, "alias": true, "try_files": true, "proxy_pass": true,
	"fastcgi_pass": true, "fastcgi_param": true, "fastcgi_index": true,
	"auth_basic": true, "auth_request": true, "ssl_certificate": true,
	"proxy_ssl_certificate": true, "proxy_set_header": true,
	"proxy_pass_request_headers": true, "proxy_pass_request_body": true,
	"mirror": true, "dav_methods": true, "create_full_put_path": true,
	"autoindex": true, "ssi": true, "client_body_temp_path": true,
	"client_body_in_file_only": true, "proxy_store": true, "fastcgi_store": true,
}

// deniedPrefixes cover the scripting modules (Lua, Perl, njs).
var deniedPrefixes = []string{"lua_", "perl", "js_", "njs_", "set_by_lua", "content_by_lua", "access_by_lua", "rewrite_by_lua", "header_filter_by_lua", "body_filter_by_lua", "log_by_lua", "init_by_lua", "init_worker_by_lua", "ssl_certificate_by_lua"}

// allowedBlocks are the only block directives a snippet may open; it is
// included at server level.
var allowedBlocks = map[string]bool{"location": true, "if": true, "limit_except": true}

// ValidateSnippet checks a custom nginx snippet for size, structure and
// dangerous directives. It is a deny-list on top of a size limit and a
// block allow-list, not a full nginx parser: `nginx -t` in the build is the
// final check, and the plan says so.
func ValidateSnippet(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > MaxSnippetBytes {
		return invalid("custom nginx snippet exceeds %d bytes", MaxSnippetBytes)
	}
	if !utf8.ValidString(s) {
		return invalid("custom nginx snippet is not valid UTF-8")
	}
	if strings.Count(s, "\n")+1 > MaxSnippetLines {
		return invalid("custom nginx snippet exceeds %d lines", MaxSnippetLines)
	}
	for _, r := range s {
		if r == 0 || (r < 0x20 && r != '\n' && r != '\t') {
			return invalid("custom nginx snippet contains control characters")
		}
	}
	stmts, err := splitStatements(s)
	if err != nil {
		return err
	}
	depth := 0
	for _, st := range stmts {
		switch st.end {
		case '}':
			if st.text != "" {
				return invalid("custom nginx snippet: directive %q is missing a ';'", firstWord(st.text))
			}
			depth--
			if depth < 0 {
				return invalid("custom nginx snippet has an unmatched '}'")
			}
			continue
		}
		name := strings.ToLower(firstWord(st.text))
		if name == "" {
			return invalid("custom nginx snippet has an empty directive")
		}
		if deniedDirectives[name] {
			return invalid("custom nginx snippet: directive %q is not allowed", name)
		}
		for _, p := range deniedPrefixes {
			if strings.HasPrefix(name, p) {
				return invalid("custom nginx snippet: directive %q is not allowed", name)
			}
		}
		if st.end == '{' {
			if !allowedBlocks[name] {
				return invalid("custom nginx snippet: block %q is not allowed (location, if, limit_except only)", name)
			}
			depth++
		}
	}
	if depth != 0 {
		return invalid("custom nginx snippet has an unclosed '{'")
	}
	return nil
}

type statement struct {
	text string
	end  byte // ';', '{' or '}'
}

// splitStatements tokenizes at ';', '{' and '}' outside of quotes and
// comments. Text left over at the end without a terminator is an error.
func splitStatements(s string) ([]statement, error) {
	var out []statement
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\n' || c == '\r' {
				return nil, invalid("custom nginx snippet has a multiline quoted value")
			}
			cur.WriteByte(c)
			if c == '\\' && i+1 < len(s) {
				if s[i+1] == '\n' || s[i+1] == '\r' {
					return nil, invalid("custom nginx snippet has a multiline quoted value")
				}
				i++
				cur.WriteByte(s[i])
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
			cur.WriteByte(c)
		case c == '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			cur.WriteByte(' ')
		case c == ';' || c == '{' || c == '}':
			out = append(out, statement{text: strings.TrimSpace(cur.String()), end: c})
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, invalid("custom nginx snippet has an unterminated quote")
	}
	if strings.TrimSpace(cur.String()) != "" {
		return nil, invalid("custom nginx snippet: directive %q is missing a ';'", firstWord(strings.TrimSpace(cur.String())))
	}
	return out, nil
}

func firstWord(s string) string {
	if i := strings.IndexAny(s, " \t\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
