// Package fluentbird holds the theme files so the installer can embed them.
// The CSS and icons live at the top of the repository, exactly where Fluentbird
// keeps them, so manual installs and upstream updates keep working.
package fluentbird

import "embed"

//go:embed userChrome.css custom.css Icons Titlebar_Icons
var Files embed.FS
