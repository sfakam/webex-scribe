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
	set choices to {"Sync Transcripts locally — last 7 days", "Sync Transcripts locally — last 30 days", "Sync Transcripts to Google Drive — last 7 days", "Sync Transcripts to Google Drive — last 30 days", "Update Calendar — next 30 days"}
	set chosen to choose from list choices with prompt "Webex Scribe — What would you like to do?" default items {item 1 of choices} without multiple selections allowed
	if chosen is false then return
	set action to item 1 of chosen

	-- Build command.
	if action is "Sync Transcripts locally — last 7 days" then
		set cmd to quoted form of binaryPath & " --local --days 7"
	else if action is "Sync Transcripts locally — last 30 days" then
		set cmd to quoted form of binaryPath & " --local --days 30"
	else if action is "Sync Transcripts to Google Drive — last 7 days" then
		set cmd to quoted form of binaryPath & " --days 7"
	else if action is "Sync Transcripts to Google Drive — last 30 days" then
		set cmd to quoted form of binaryPath & " --days 30"
	else
		set cmd to quoted form of binaryPath & " --calendar 30"
	end if

	set homeDir to POSIX path of (path to home folder)
	set fullCmd to "export HOME=" & quoted form of homeDir & " && " & cmd & " 2>&1; echo \"EXIT:$?\""

	display notification "Running…" with title "Webex Scribe"

	runCommand(binaryPath, fullCmd, homeDir)
end run

on runCommand(binaryPath, fullCmd, homeDir)
	try
		set rawOutput to do shell script fullCmd

		-- Extract exit code from sentinel line.
		set exitCode to 0
		set outputLines to paragraphs of rawOutput
		repeat with i from (count outputLines) to 1 by -1
			set ln to item i of outputLines
			if ln starts with "EXIT:" then
				set exitCode to (text 6 thru -1 of ln) as integer
				set outputLines to items 1 thru (i - 1) of outputLines
				exit repeat
			end if
		end repeat
		set output to outputLines as text

		if exitCode is 2 then
			-- Webex token expired.
			handleWebexTokenExpired(binaryPath, fullCmd, homeDir)
		else if exitCode is 3 then
			-- Google auth required.
			handleGoogleAuth(binaryPath, fullCmd, homeDir)
		else
			display dialog output buttons {"OK"} default button "OK" with title "Webex Scribe — Done"
		end if

	on error errMsg
		display alert "Unexpected error" message errMsg as critical
	end try
end runCommand

on handleWebexTokenExpired(binaryPath, fullCmd, homeDir)
	set dlg to display dialog "Your Webex token has expired. Paste a new Personal Access Token from developer.webex.com (valid 12 hours):" default answer "" buttons {"Cancel", "Submit"} default button "Submit" with title "Webex Token Expired"
	if button returned of dlg is "Cancel" then return
	set token to text returned of dlg
	if token is "" then return
	set newCmd to fullCmd & " --token " & quoted form of token
	try
		set output to do shell script newCmd
		display dialog output buttons {"OK"} default button "OK" with title "Webex Scribe — Done"
	on error err2
		display alert "Error" message err2 as critical
	end try
end handleWebexTokenExpired

on handleGoogleAuth(binaryPath, fullCmd, homeDir)
	display dialog "Your Google / Drive authentication needs to be refreshed. A browser window will open for you to sign in with your Google account. Click OK to continue." buttons {"Cancel", "OK"} default button "OK" with title "Google Sign-In Required"
	if button returned of result is "Cancel" then return
	try
		do shell script "export HOME=" & quoted form of homeDir & " && gcloud auth login --enable-gdrive-access 2>&1"
		display notification "Google sign-in complete. Retrying…" with title "Webex Scribe"
		-- Retry the original command now that auth is refreshed.
		set output to do shell script fullCmd
		display dialog output buttons {"OK"} default button "OK" with title "Webex Scribe — Done"
	on error err2
		if err2 contains "gcloud: command not found" then
			display alert "gcloud not found" message "The Google Cloud CLI (gcloud) is not installed. Please install it from cloud.google.com/sdk and then re-run Webex Scribe." as critical
		else
			display alert "Google sign-in failed" message err2 as critical
		end if
	end try
end handleGoogleAuth
