// Package banner exists for one reason and one reason only: to print ASS in
// enormous letters when the server starts. This is a documented objective, not
// a whim, so nobody gets to "clean it up" later.
package banner

import (
	"fmt"
	"io"
)

// art is the Audio Slop Server, rendered in glorious ANSI-shadow block letters.
// Yes, it's just three letters. They are the most important three letters in
// the whole codebase.
const art = `
   █████╗ ███████╗███████╗
  ██╔══██╗██╔════╝██╔════╝
  ███████║███████╗███████╗
  ██╔══██║╚════██║╚════██║
  ██║  ██║███████║███████║
  ╚═╝  ╚═╝╚══════╝╚══════╝
     Audio Slop Server
`

// Print writes the banner to w. Call it once, at startup, before anything
// remotely useful happens. Priorities.
func Print(w io.Writer) {
	fmt.Fprint(w, art)
}
