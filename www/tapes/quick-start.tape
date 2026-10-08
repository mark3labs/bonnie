# Real BONNIE CLI and live openai/gpt-6.1-sol conversation.
# Run with ./www/tapes/record.sh from the repository root.
Output ../public/quick-start.gif

Set Shell "bash"
Set FontFamily "DejaVu Sans Mono"
Set FontSize 18
Set Width 1100
Set Height 720
Set Padding 20
Set Framerate 12
Set TypingSpeed 45ms
Set Theme { "name": "bonnie", "background": "#080b20", "foreground": "#f4f5ff", "cursor": "#00cbea", "black": "#080b20", "red": "#ff668f", "green": "#7ce6bb", "yellow": "#ffc478", "blue": "#62e8ff", "magenta": "#ff64cf", "cyan": "#00cbea", "white": "#f4f5ff", "brightBlack": "#9faecc", "brightRed": "#ff668f", "brightGreen": "#7ce6bb", "brightYellow": "#ffc478", "brightBlue": "#62e8ff", "brightMagenta": "#ff64cf", "brightCyan": "#62e8ff", "brightWhite": "#ffffff" }

# Set the temporary workspace and prompt without exposing credentials.
Hide
Type `export PATH="$BONNIE_DEMO_BIN:$PATH"; export PS1='$ '; cd "$BONNIE_DEMO_DIR"; clear` Enter
Sleep 1s
Show

Type "bonnie init hello-bonnie --model openai/gpt-6.1-sol" Enter
Wait+Screen@30s /instructions.md/
Sleep 1s
Type "cd hello-bonnie" Enter
# Show that personality belongs in the authored instructions file.
Type@15ms `printf '%s\n' 'You are Bonnie, the witty engineer at FLAG in Knight Rider.' 'Call the user Michael. Keep replies to two short sentences.' 'Discuss missions without tools. Safety clearance comes before turbo boost.' > instructions.md` Enter
Sleep 1s
Type "cat instructions.md" Enter
Wait+Screen@30s /Safety clearance comes before turbo boost\./
Sleep 2s
Type "go mod tidy" Enter
Wait+Screen@60s /(?s)\$ go mod tidy.*\n\$\s/
Sleep 1s
Type "bonnie dev" Enter
Wait+Screen@120s /Type a message or/
Sleep 1s
# Keep this a live conversation, with short replies that fit on screen.
Type "Bonnie, KITT crashed mid-mission! How do durable agents save the day? End with OVER." Enter
Wait+Screen@120s /(?s)OVER\..*OVER.*done/
Sleep 4s

Type "KITT is back, but wants turbo boost before approval. Give him a witty one-line safety briefing. End with ROGER." Enter
Wait+Screen@120s /(?s)ROGER\..*ROGER.*done/
Sleep 6s

# Close the development server without recording the exit sequence.
Hide
Type "/exit" Enter
Sleep 2s
