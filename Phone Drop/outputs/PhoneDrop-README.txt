# PhoneDrop for Windows

PhoneDrop is a portable Windows app that opens a local file-transfer page and serves it directly to phones on the same Wi-Fi network. Phones use their normal browsers; there is no Android app, account, or cloud service. The compiled app includes the Go runtime, so Go does not need to be installed on a PC to run `PhoneDrop.exe`.

## Run

1. To build from source, install Go 1.22 or newer and double-click `Build-Windows.cmd` (or run `powershell -ExecutionPolicy Bypass -File .\Build-Windows.ps1`).
2. Start `outputs\PhoneDrop.exe`. It opens the browser and runs in the background without a console window. The app exits automatically after the last PhoneDrop page closes. The portable executable does not need a Go installation.
3. Connect the phone and PC to the same trusted Wi-Fi. Scan the QR shown in the browser. If Windows Firewall asks, allow PhoneDrop on **Private networks**.
4. Upload files from either device. They appear in `%USERPROFILE%\Downloads\Phone Drop` and can be downloaded from either device.

The app shows the PC address and an eight-digit pairing code on the page. The code changes each time PhoneDrop starts. The QR and code are visible on mobile too, so another phone can scan the same link. For an older phone camera that does not recognize the QR, type the address shown on the page into the phone browser.

Files are stored in `%USERPROFILE%\Downloads\Phone Drop`. The dark-mode switch saves its setting to `.phonedrop-settings.json` in that folder. **Clear cache** asks before deleting the files available to download and preserves that settings file.

## Notes

- PhoneDrop starts at port 8765 and automatically tries the next 15 ports if it is occupied. Use `PhoneDrop.exe -port 9000` to choose a different starting port.
- **Stop transfers** pauses active uploads and downloads; the button becomes **Start transfers** to continue them. Closing the last PhoneDrop page exits the app; if a browser crashes without sending its close signal, a five-minute heartbeat timeout cleans it up.
- If the default port is occupied, PhoneDrop tries the next 15 ports automatically.
- To use a different shared folder, start `PhoneDrop.exe -folder "D:\Shared Files"`.
- File transfer stays on the local network. Use a trusted Wi-Fi network and keep the pairing QR/code private.
- The Windows executable uses the supplied logo as its file icon. Its console window is disabled.
- The app and QR encoder use Go's standard library, the included HTML page, and the two supplied cloud-arrow SVGs. The Go MSI is only needed as a build tool and is not installed or bundled for users of the portable executable.
