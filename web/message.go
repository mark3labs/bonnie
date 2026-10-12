package web

type message struct {
	Role, Text, Payload, HTML string
	Tools                     []toolActivity
	Parts                     []messagePart
}

type messagePart struct {
	HTML, Text string
	Tool       *toolActivity
}

type toolActivity struct {
	Name, Kind, Summary, Detail string
	Sections                    []toolSection
}

// HTML contains only escaped text and fixed syntax-color spans.
type toolSection struct{ Label, Text, HTML string }
