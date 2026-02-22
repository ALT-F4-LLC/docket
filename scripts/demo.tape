# Docket CLI Demo
# Run: vhs demo.tape
# Produces: docs/demo.gif

Output docs/demo.gif

Require docket

# --- Terminal settings ---
Set Shell "bash"
Set FontSize 16
Set Width 1400
Set Height 800
Set Padding 20
Set Framerate 30
Set PlaybackSpeed 1.0
Set TypingSpeed 25ms
Set Theme "Catppuccin Mocha"
Set WindowBar Colorful
Set CursorBlink false

# --- Setup: build binary and prepare clean workspace ---
Hide
Type "cd $(mktemp -d) && export PATH=$OLDPWD/bin:$PATH"
Enter
Sleep 500ms
Type "clear"
Enter
Sleep 500ms
Show

# --- Scene 1: Initialize ---
Sleep 300ms
Type "# Initialize docket in a new project"
Enter
Sleep 200ms
Type "docket init"
Enter
Sleep 500ms

# --- Scene 2: Create issues (an epic with sub-tasks) ---
Type "# Create an epic with sub-tasks"
Enter
Sleep 200ms
Type "docket issue create -t 'Build user authentication' -p critical -T epic"
Enter
Sleep 500ms

Type "docket issue create -t 'Design auth database schema' -p high -T task --parent DKT-1"
Enter
Sleep 300ms

Type "docket issue create -t 'Implement JWT middleware' -p high -T task --parent DKT-1"
Enter
Sleep 300ms

# --- Scene 3: List issues ---
Type "# List all issues"
Enter
Sleep 200ms
Type "docket issue list"
Enter
Sleep 500ms

# --- Scene 4: Attach files to issues ---
Type "# Attach files to issues"
Enter
Sleep 200ms
Type "docket issue file add DKT-2 schema.sql migrations/"
Enter
Sleep 300ms

# --- Scene 5: List files on an issue ---
Type "# List files attached to an issue"
Enter
Sleep 200ms
Type "docket issue file list DKT-2"
Enter
Sleep 500ms

# --- Scene 6: Set up dependencies ---
Type "# Link dependencies between issues"
Enter
Sleep 200ms
Type "docket issue link add DKT-3 depends-on DKT-2"
Enter
Sleep 300ms

# --- Scene 7: Dependency graph ---
Type "# View the dependency graph"
Enter
Sleep 200ms
Type "docket issue graph DKT-3"
Enter
Sleep 500ms

# --- Scene 8: Move some issues to show workflow ---
Type "# Move issues through the workflow"
Enter
Sleep 200ms
Type "docket issue move DKT-2 todo"
Enter
Sleep 300ms

# --- Scene 9: Board view ---
Type "# View the Kanban board"
Enter
Sleep 200ms
Type "docket board --expand"
Enter
Sleep 500ms

# --- Scene 10: Next (work-ready items) ---
Type "# What should I work on next?"
Enter
Sleep 200ms
Type "docket next"
Enter
Sleep 500ms

# --- Scene 11: Execution plan ---
Type "# Generate an execution plan"
Enter
Sleep 200ms
Type "docket plan --root DKT-1"
Enter
Sleep 500ms

# --- Final pause ---
Sleep 500ms
