package explorer

import (
	"net/http"
	"net/url"
)

// The theme can be switched **without JavaScript** too.
//
// The switcher is an ordinary `?theme=light|dark` link. With JS it is handled by /theme.js from the
// site's shared header (right in the browser, without loading the page); without JS it goes through
// this server. The server remembers the choice in a cookie, redirects back to the same address without
// the parameter (so it can be passed on) and writes `data-theme` on `<html>`. The tokens in the CSS
// take care of the rest.
//
// The choice is in a cookie, not in localStorage: both the site and the explorer read it, so it
// carries across the whole domain. The explorer also reads it on the server, so the page does not
// flash the wrong theme.
//
// Without the cookie `data-theme` is not written at all and `prefers-color-scheme` applies — that is,
// the system setting, which is the right answer for a first visit.
const themeCookie = "theme"

func validTheme(v string) bool { return v == "light" || v == "dark" }

// themeOf returns the chosen theme, or an empty string when the visitor has not chosen one.
func themeOf(r *http.Request) string {
	c, err := r.Cookie(themeCookie)
	if err != nil || !validTheme(c.Value) {
		return ""
	}
	return c.Value
}

// withTheme intercepts `?theme=`, stores the choice and redirects to a clean address.
func withTheme(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		v := q.Get(themeCookie)
		if v == "" {
			next.ServeHTTP(w, r)
			return
		}
		if validTheme(v) {
			http.SetCookie(w, &http.Cookie{
				Name:  themeCookie,
				Value: v,
				Path:  "/",
				// A year is enough; it is a display preference, not a login.
				MaxAge: 365 * 24 * 3600,
				// Deliberately WITHOUT HttpOnly. The same cookie is read and written by /theme.js on the
				// site's static pages — with HttpOnly a choice made in the explorer would not carry to the
				// homepage. It is a display preference, nothing worth stealing.
				HttpOnly: false,
				SameSite: http.SameSiteLaxMode,
				// Behind a reverse proxy, HTTPS can only be learned from a header.
				Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
			})
		}
		q.Del(themeCookie)
		u := *r.URL
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
	})
}

// themeLink assembles the link that switches the theme for the current address.
func themeLink(r *http.Request, to string) string {
	q := r.URL.Query()
	q.Set(themeCookie, to)
	u := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	return u.RequestURI()
}
