package web

import "embed"

//go:embed login.html account.html styles.css
var Assets embed.FS
