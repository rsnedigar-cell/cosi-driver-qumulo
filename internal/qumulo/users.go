package qumulo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// qint accepts a JSON number, a numeric string, or "".
type qint int

func (q *qint) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "" {
		*q = 0
		return nil
	}
	if len(s) > 0 && s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		if str == "" {
			*q = 0
			return nil
		}
		n, err := strconv.Atoi(str)
		if err != nil {
			return err
		}
		*q = qint(n)
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	*q = qint(n)
	return nil
}

// LocalUser is a Qumulo local auth user.
type LocalUser struct {
	ID           string `json:"id,omitempty"`
	Name         string `json:"name"`
	PrimaryGroup string `json:"primary_group,omitempty"`
	SID          string `json:"sid,omitempty"`
	UID          qint   `json:"uid,omitempty"`
	HomeDir      string `json:"home_directory,omitempty"`
}

type LocalGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type createUserRequest struct {
	Name         string `json:"name"`
	Password     string `json:"password,omitempty"`
	PrimaryGroup string `json:"primary_group,omitempty"`
	UID          *int   `json:"uid,omitempty"`
}

// DefaultUsersGroupID is the local Users RID on Qumulo (same as Windows 513).
const DefaultUsersGroupID = "513"

func (c *Connection) CreateUser(ctx context.Context, name, unusablePassword string) (*LocalUser, error) {
	gid, gerr := c.UsersGroupID(ctx)
	if gerr != nil || gid == "" {
		gid = DefaultUsersGroupID
	}
	out, err := c.createUser(ctx, name, unusablePassword, gid)
	if err != nil && primaryGroupRejected(err) {
		return c.createUser(ctx, name, unusablePassword, "")
	}
	return out, err
}

func (c *Connection) createUser(ctx context.Context, name, password, primaryGroup string) (*LocalUser, error) {
	var out LocalUser
	_, err := c.DoJSON(ctx, http.MethodPost, "/v1/users/", nil, nil, createUserRequest{
		Name:         name,
		Password:     password,
		PrimaryGroup: primaryGroup,
	}, &out)
	if err != nil {
		return nil, err
	}
	if out.Name == "" {
		out.Name = name
	}
	return &out, nil
}

func primaryGroupRejected(err error) bool {
	api, ok := AsAPIError(err)
	if !ok {
		return false
	}
	return strings.Contains(strings.ToLower(api.Description), "primary_group")
}

func (c *Connection) UsersGroupID(ctx context.Context) (string, error) {
	groups, err := c.ListGroups(ctx)
	if err != nil {
		return "", err
	}
	for _, g := range groups {
		if strings.EqualFold(g.Name, "Users") {
			return g.ID, nil
		}
	}
	return DefaultUsersGroupID, nil
}

func (c *Connection) ListGroups(ctx context.Context) ([]LocalGroup, error) {
	var raw json.RawMessage
	_, err := c.DoJSON(ctx, http.MethodGet, "/v1/groups/", nil, nil, nil, &raw)
	if err != nil {
		return nil, err
	}
	return decodeGroupList(raw)
}

func decodeGroupList(raw json.RawMessage) ([]LocalGroup, error) {
	var list []LocalGroup
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var wrap struct {
		Groups  []LocalGroup `json:"groups"`
		Entries []LocalGroup `json:"entries"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, fmt.Errorf("decode groups: %w", err)
	}
	if len(wrap.Groups) > 0 {
		return wrap.Groups, nil
	}
	return wrap.Entries, nil
}

func (c *Connection) ListUsers(ctx context.Context) ([]LocalUser, error) {
	var raw json.RawMessage
	_, err := c.DoJSON(ctx, http.MethodGet, "/v1/users/", nil, nil, nil, &raw)
	if err != nil {
		return nil, err
	}
	return decodeUserList(raw)
}

func decodeUserList(raw json.RawMessage) ([]LocalUser, error) {
	// Core 7.9.2.2 returns a bare array. Older cores wrap {users|entries}.
	var list []LocalUser
	if err := json.Unmarshal(raw, &list); err == nil && (len(list) > 0 || string(raw) == "[]") {
		return list, nil
	}
	var wrap struct {
		Users   []LocalUser `json:"users"`
		Entries []LocalUser `json:"entries"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, fmt.Errorf("decode users: %w", err)
	}
	if len(wrap.Users) > 0 {
		return wrap.Users, nil
	}
	return wrap.Entries, nil
}

func (c *Connection) GetUserByName(ctx context.Context, name string) (*LocalUser, error) {
	users, err := c.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if users[i].Name == name {
			return &users[i], nil
		}
	}
	return nil, &APIError{StatusCode: http.StatusNotFound, ErrorClass: ErrClassAuthNoSuchUser, Description: fmt.Sprintf("user %q not found", name)}
}

func (c *Connection) DeleteUser(ctx context.Context, idOrName string) error {
	// Prefer numeric id if it looks like one; otherwise resolve.
	pth := idOrName
	if _, err := strconv.Atoi(idOrName); err != nil {
		u, gerr := c.GetUserByName(ctx, idOrName)
		if gerr != nil {
			return gerr
		}
		if u.ID != "" {
			pth = u.ID
		} else {
			pth = idOrName
		}
	}
	return c.DeleteUserByID(ctx, pth)
}

// DeleteUserByID deletes exactly one immutable auth identity without a
// second name lookup that could race with delete-and-recreate of that name.
func (c *Connection) DeleteUserByID(ctx context.Context, authID string) error {
	_, err := c.DoJSON(ctx, http.MethodDelete, "/v1/users/"+url.PathEscape(authID), nil, nil, nil, nil)
	return err
}
