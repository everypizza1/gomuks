// gomuks - A Matrix client written in Go.
// Copyright (C) 2024 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/hlog"
	"go.mau.fi/util/exerrors"
	"go.mau.fi/util/exhttp"
	"go.mau.fi/util/jsontime"
	"go.mau.fi/util/requestlog"
	"golang.org/x/crypto/bcrypt"
	"maunium.net/go/mautrix"

	"go.mau.fi/gomuks/web"
)

func (gmx *Gomuks) StartServer() {
	api := http.NewServeMux()
	api.HandleFunc("GET /websocket", gmx.HandleWebsocket)
	api.HandleFunc("POST /auth", gmx.Authenticate)
	api.HandleFunc("GET /media/{server}/{media_id}", gmx.DownloadMedia)
	apiHandler := exhttp.ApplyMiddleware(
		api,
		hlog.NewHandler(*gmx.Log),
		hlog.RequestIDHandler("request_id", "Request-ID"),
		requestlog.AccessLogger(false),
		exhttp.StripPrefix("/_gomuks"),
		gmx.AuthMiddleware,
	)
	router := http.NewServeMux()
	router.Handle("/_gomuks/", apiHandler)
	if frontend, err := fs.Sub(web.Frontend, "dist"); err != nil {
		gmx.Log.Warn().Msg("Frontend not found")
	} else {
		router.Handle("/", http.FileServerFS(frontend))
	}
	gmx.Server = &http.Server{
		Addr:    gmx.Config.Web.ListenAddress,
		Handler: router,
	}
	go func() {
		err := gmx.Server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			panic(err)
		}
	}()
	gmx.Log.Info().Str("address", gmx.Config.Web.ListenAddress).Msg("Server started")
}

var (
	ErrInvalidHeader = mautrix.RespError{ErrCode: "FI.MAU.GOMUKS.INVALID_HEADER", StatusCode: http.StatusBadRequest}
	ErrMissingCookie = mautrix.RespError{ErrCode: "FI.MAU.GOMUKS.MISSING_COOKIE", Err: "Missing gomuks_auth cookie", StatusCode: http.StatusUnauthorized}
	ErrInvalidCookie = mautrix.RespError{ErrCode: "FI.MAU.GOMUKS.INVALID_COOKIE", Err: "Invalid gomuks_auth cookie", StatusCode: http.StatusUnauthorized}
)

type tokenData struct {
	Username string        `json:"username"`
	Expiry   jsontime.Unix `json:"expiry"`
}

func (gmx *Gomuks) validateAuth(token string) bool {
	if len(token) > 100 {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return false
	}
	rawJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	checksum, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	hasher := hmac.New(sha256.New, []byte(gmx.Config.Web.TokenKey))
	hasher.Write(rawJSON)
	if !hmac.Equal(hasher.Sum(nil), checksum) {
		return false
	}

	var td tokenData
	err = json.Unmarshal(rawJSON, &td)
	return err == nil && td.Username == gmx.Config.Web.Username && td.Expiry.After(time.Now())
}

func (gmx *Gomuks) generateToken() (string, time.Time) {
	expiry := time.Now().Add(7 * 24 * time.Hour)
	data := exerrors.Must(json.Marshal(tokenData{
		Username: gmx.Config.Web.Username,
		Expiry:   jsontime.U(expiry),
	}))
	hasher := hmac.New(sha256.New, []byte(gmx.Config.Web.TokenKey))
	hasher.Write(data)
	checksum := hasher.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(checksum), expiry
}

func (gmx *Gomuks) writeTokenCookie(w http.ResponseWriter) {
	token, expiry := gmx.generateToken()
	http.SetCookie(w, &http.Cookie{
		Name:     "gomuks_auth",
		Value:    token,
		Expires:  expiry,
		HttpOnly: true,
	})
}

func (gmx *Gomuks) Authenticate(w http.ResponseWriter, r *http.Request) {
	authCookie, err := r.Cookie("gomuks_auth")
	if err == nil && gmx.validateAuth(authCookie.Value) {
		gmx.writeTokenCookie(w)
		w.WriteHeader(http.StatusOK)
	} else if username, password, ok := r.BasicAuth(); !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="gomuks web" charset="UTF-8"`)
		w.WriteHeader(http.StatusUnauthorized)
	} else {
		usernameHash := sha256.Sum256([]byte(username))
		expectedUsernameHash := sha256.Sum256([]byte(gmx.Config.Web.Username))
		usernameCorrect := hmac.Equal(usernameHash[:], expectedUsernameHash[:])
		passwordCorrect := bcrypt.CompareHashAndPassword([]byte(gmx.Config.Web.PasswordHash), []byte(password)) == nil
		if usernameCorrect && passwordCorrect {
			gmx.writeTokenCookie(w)
			w.WriteHeader(http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
	}
}

func isUserFetch(header http.Header) bool {
	return header.Get("Sec-Fetch-Site") == "none" &&
		header.Get("Sec-Fetch-Mode") == "navigate" &&
		header.Get("Sec-Fetch-Dest") == "document" &&
		header.Get("Sec-Fetch-User") == "?1"
}

func (gmx *Gomuks) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Fetch-Site") != "same-origin" && !isUserFetch(r.Header) {
			ErrInvalidHeader.WithMessage("Invalid Sec-Fetch-Site header").Write(w)
			return
		}
		if r.URL.Path != "/auth" {
			authCookie, err := r.Cookie("gomuks_auth")
			if err != nil {
				ErrMissingCookie.Write(w)
				return
			} else if !gmx.validateAuth(authCookie.Value) {
				ErrInvalidCookie.Write(w)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}