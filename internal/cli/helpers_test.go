package cli

import "log/slog"

func defaultHandler() slog.Handler { return slog.Default().Handler().WithAttrs(nil).WithGroup("g") }
func logWarn(msg string)           { slog.Warn(msg) }
func logInfo(msg string)           { slog.Info(msg) }
