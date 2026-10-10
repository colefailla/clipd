module github.com/colefailla/clipd

// 1.24 is the true floor: log/slog's DiscardHandler landed there. clipd has no
// third-party dependencies, so there is nothing else to constrain the version.
go 1.24
