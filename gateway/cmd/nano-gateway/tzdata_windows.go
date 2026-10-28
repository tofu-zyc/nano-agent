//go:build windows

package main

// Offline Windows installations must not depend on Go or a system IANA database.
import _ "time/tzdata"
