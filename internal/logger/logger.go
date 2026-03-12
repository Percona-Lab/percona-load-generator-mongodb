package logger

import (
	"fmt"
	"strings"
)

// ANSI Color Codes
const (
	Reset  = "\033[0m"
	Red    = "\033[31m"
	Green  = "\033[32m"
	Yellow = "\033[33m"
	Blue   = "\033[34m"
	Purple = "\033[35m"
	Cyan   = "\033[36m"
	Gray   = "\033[37m"
	Bold   = "\033[1m"
)

// Info prints a formatted info message with the [INFO] tag in Green.
func Info(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	// Indent multiline messages to align with the text, not the tag
	msg = strings.ReplaceAll(msg, "\n", "\n         ")
	fmt.Printf("  %s %s%s\n", Green+"[INFO]"+Reset, msg, Reset)
}

// Color helpers for other packages
func CyanString(format string, args ...interface{}) string {
	return Cyan + fmt.Sprintf(format, args...) + Reset
}

func GreenString(format string, args ...interface{}) string {
	return Green + fmt.Sprintf(format, args...) + Reset
}

func RedString(format string, args ...interface{}) string {
	return Red + fmt.Sprintf(format, args...) + Reset
}

func BoldString(format string, args ...interface{}) string {
	return Bold + fmt.Sprintf(format, args...) + Reset
}

func YellowString(format string, args ...interface{}) string {
	return Yellow + fmt.Sprintf(format, args...) + Reset
}

// Header prints a bordered header message (Cyan).
func Header(msg string) {
	fmt.Println()
	fmt.Printf("  %s\n", CyanString("--- %s ---", msg))
	fmt.Println()
}

// Section prints a smaller section separator.
func Section(msg string) {
	fmt.Printf("\n  %s %s\n", GreenString(">"), msg)
}

// Debug prints a formatted debug message with the [DEBUG] tag in Purple.
func Debug(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	// Indent multiline messages to align with the text
	msg = strings.ReplaceAll(msg, "\n", "\n          ")
	fmt.Printf("  %s %s%s\n", Purple+"[DEBUG]"+Reset, msg, Reset)
}
