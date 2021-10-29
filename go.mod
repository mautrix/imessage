module go.mau.fi/mautrix-imessage

go 1.14

require (
	github.com/fsnotify/fsnotify v1.5.1
	github.com/gabriel-vasile/mimetype v1.4.0
	github.com/mattn/go-sqlite3 v1.14.9
	gopkg.in/yaml.v2 v2.4.0
	maunium.net/go/mauflag v1.0.0
	maunium.net/go/maulogger/v2 v2.3.1
	maunium.net/go/mautrix v0.9.32-0.20211029160607-edff210dd28b
)

// Newer golang.org/x/sys versions break darwin/arm32
replace golang.org/x/sys => golang.org/x/sys v0.0.0-20201119102817-f84b799fce68
