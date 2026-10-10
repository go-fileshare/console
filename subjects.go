// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"errors"
	"fmt"
	"strings"

	v1 "github.com/go-fileshare/console/internal/adminpb"
)

// parseSubject reads a grant's subject in the vocabulary of fileshare's
// configuration file: a person ("alice"), @group, oidc:groups:<value> or
// oidc:user:<name>.
func parseSubject(s string) (*v1.Subject, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return nil, errors.New("whom: a person, @group, oidc:groups:<group> or oidc:user:<name>")
	case strings.HasPrefix(s, "@"):
		if len(s) == 1 {
			return nil, errors.New("@ names a group: @staff")
		}
		return &v1.Subject{Kind: &v1.Subject_Group{Group: s[1:]}}, nil
	case strings.HasPrefix(s, "oidc:groups:"):
		g := strings.TrimPrefix(s, "oidc:groups:")
		if g == "" {
			return nil, errors.New("oidc:groups: needs a group")
		}
		return &v1.Subject{Kind: &v1.Subject_OidcGroup{OidcGroup: g}}, nil
	case strings.HasPrefix(s, "oidc:user:"):
		u := strings.TrimPrefix(s, "oidc:user:")
		if u == "" {
			return nil, errors.New("oidc:user: needs a name")
		}
		return &v1.Subject{Kind: &v1.Subject_OidcUser{OidcUser: u}}, nil
	case strings.HasPrefix(s, "oidc:"):
		return nil, fmt.Errorf("%q: oidc:groups:<group> or oidc:user:<name>", s)
	}
	return &v1.Subject{Kind: &v1.Subject_User{User: s}}, nil
}

// formatSubject is parseSubject's inverse.
func formatSubject(s *v1.Subject) string {
	switch k := s.GetKind().(type) {
	case *v1.Subject_Group:
		return "@" + k.Group
	case *v1.Subject_OidcGroup:
		return "oidc:groups:" + k.OidcGroup
	case *v1.Subject_OidcUser:
		return "oidc:user:" + k.OidcUser
	}
	return s.GetUser()
}

// grantLine is how a grant is listed.
func grantLine(g *v1.Grant) string {
	acc := "read"
	if g.GetAccess() == v1.Access_ACCESS_WRITE {
		acc = "write"
	}
	return formatSubject(g.GetSubject()) + " (" + acc + ")"
}

// source is what a share serves.
func source(s *v1.Share) string {
	switch {
	case s.GetDirectory() != "":
		return s.GetDirectory()
	case s.GetImage() != "":
		return s.GetImage()
	case s.GetVolume() != nil:
		return "volume " + s.GetVolume().GetParent() + "/" + s.GetVolume().GetName()
	}
	return "?"
}

// shareLine is how a share is listed.
func shareLine(s *v1.Share) string {
	line := s.GetName() + "  " + source(s)
	if !s.GetEnabled() {
		line += "  (disabled)"
	}
	return line
}

// shareDetails are the lines that describe a share.
func shareDetails(s *v1.Share) []string {
	kind := "directory"
	switch {
	case s.GetImage() != "":
		kind = "disk image"
	case s.GetVolume() != nil:
		kind = "volume"
	}
	out := []string{kind + ": " + source(s)}
	switch s.GetOrigin() {
	case v1.Origin_ORIGIN_CONFIG:
		out = append(out, "defined in the configuration file: only enabling and disabling apply here")
	case v1.Origin_ORIGIN_API:
		out = append(out, "created through the admin API")
	}
	if !s.GetEnabled() {
		out = append(out, "disabled: nobody can connect")
	}
	if s.GetFilesystem() != "" {
		out = append(out, "filesystem: "+s.GetFilesystem())
	}
	switch {
	case s.GetEffectiveReadOnly() && !s.GetReadOnly() && !anyWrite(s):
		out = append(out, "served read-only: nobody is granted write")
	case s.GetEffectiveReadOnly() && !s.GetReadOnly():
		out = append(out, "served read-only: its source cannot take a write")
	case s.GetReadOnly():
		out = append(out, "read-only")
	}
	if n := s.GetSizeBytes(); n > 0 {
		out = append(out, "size: "+bytesString(n))
	}
	if p := s.GetProtocols(); len(p) > 0 {
		out = append(out, "protocols: "+strings.Join(p, ", "))
	}
	if p := s.GetServedOver(); len(p) > 0 {
		out = append(out, "served over: "+strings.Join(p, ", "))
	}
	for _, r := range s.GetRefusals() {
		out = append(out, "not over "+r.GetProtocol()+": "+r.GetReason())
	}
	return out
}

// bytesString is n in binary units.
func bytesString(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// anyWrite says whether a grant of s gives write access.
func anyWrite(s *v1.Share) bool {
	for _, g := range s.GetGrants() {
		if g.GetAccess() == v1.Access_ACCESS_WRITE {
			return true
		}
	}
	return false
}
