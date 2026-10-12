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

type toolActivity struct{ Name, Kind, Summary, Detail string }
