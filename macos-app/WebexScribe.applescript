-- Webex Scribe macOS App
-- Wraps the webex-scribe CLI with a simple choose-from-list dialog.

on run
	-- Prefer bundled binary, fall back to system PATH.
	set myPath to POSIX path of (path to me)
	set bundledBinary to myPath & "Contents/Resources/webex-scribe"
	set binaryPath to ""

	try
		do shell script "test -x " & quoted form of bundledBinary
		set binaryPath to bundledBinary
	on error
		try
			set binaryPath to do shell script "which webex-scribe"
		on error
			display alert "webex-scribe not found" message "The webex-scribe binary could not be found inside the app bundle or on your PATH. Please reinstall." as critical
			return
		end try
	end try

	-- Choose action.
	set choices to {"Sync Transcripts — last 7 days", "Sync Transcripts — last 30 days", "Update Calendar — next 30 days"}
	set chosen to choose from list choices with prompt "Webex Scribe — What would you like to do?" default items {item 1 of choices} without multiple selections allowed
	if chosen is false then return
	set action to item 1 of chosen

	-- Build command.
	if action is "Sync Transcripts — last 7 days" then
		set cmd to quoted form of binaryPath & " --local --days 7"
	else if action is "Sync Transcripts — last 30 days" then
		set cmd to quoted form of binaryPath & " --local --days 30"
	else
		set cmd to quoted form of binaryPath & " --calendar 30"
	end if

	set homeDir to POSIX path of (path to home folder)
	set fullCmd to "export HOME=" & quoted form of homeDir & " && " & cmd & " 2>&1"

	display notification "Running…" with title "Webex Scribe"

	try
		set output to do shell script fullCmd
		display dialog output buttons {"OK"} default button "OK" with title "Webex Scribe — Done"

	on error errMsg
		if errMsg contains "WEBEX_TOKEN_EXPIRED" then
			set dlg to display dialog "Your Webex token has expired. Paste a new Personal Access Token (from developer.webex.com):" default answer "" buttons {"Cancel", "Submit"} default button "Submit" with title "Webex Token Expired"
			if button returned of dlg is "Cancel" then return
			set token to text returned of dlg
			if token is "" then return
			try
				set output to do shell script fullCmd & " --token " & quoted form of token
				display dialog output buttons {"OK"} default button "OK" with title "Webex Scribe — Done"
			on error err2
				display alert "Error" message err2 as critical
			end try
		else
			display alert "Error running Webex Scribe" message errMsg as critical
		end if
	end try
end run
