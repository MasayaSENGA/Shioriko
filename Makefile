# Shioriko - mount an Android (MTP) device as a Finder volume on macOS.

VERSION  ?= 1.0.0
BIN      := shioriko
PROBE    := shioriko-probe
APP      := build/Shioriko.app
APPDIR   ?= $(HOME)/Applications
PREFIX   ?= $(HOME)/.local
BINDIR   := $(PREFIX)/bin
GO       ?= go
LDFLAGS  := -s -w -X main.version=$(VERSION)
# Code signing identity for the app bundle. "-" signs ad hoc, which is enough
# to run the app on the Mac that built it.
SIGN_IDENTITY ?= -
# A real identity (Developer ID) also gets the hardened runtime and a secure
# timestamp, which notarization requires.
SIGN_FLAGS = $(if $(filter-out -,$(SIGN_IDENTITY)),--options runtime --timestamp)
# The bundled libusb (from Homebrew) is built for the running macOS, so the
# app requires at least this macOS major version.
MIN_MACOS ?= $(shell sw_vers -productVersion | cut -d. -f1).0
LIBUSB_DIR = $(shell pkg-config --variable=libdir libusb-1.0)

.PHONY: all build probe app dist test vet clean install uninstall run run-headless icon \
	autostart-install autostart-uninstall

all: build probe app

# Plain binary, linked against Homebrew's libusb (for the terminal and scripts).
build:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/$(BIN) ./cmd/shioriko

probe:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/$(PROBE) ./cmd/shioriko-probe

# Menu bar app bundle with libusb bundled inside, so it runs without Homebrew.
app:
	rm -rf "$(APP)"
	mkdir -p "$(APP)/Contents/MacOS" "$(APP)/Contents/Resources" "$(APP)/Contents/Frameworks"
	$(GO) build -ldflags "$(LDFLAGS) -extldflags -headerpad_max_install_names" \
		-o "$(APP)/Contents/MacOS/Shioriko" ./cmd/shioriko
	sed -e 's/@VERSION@/$(VERSION)/g' -e 's/@MIN_MACOS@/$(MIN_MACOS)/g' \
		packaging/Info.plist > "$(APP)/Contents/Info.plist"
	cp packaging/AppIcon.icns "$(APP)/Contents/Resources/AppIcon.icns"
	cp internal/mtp/LICENSE "$(APP)/Contents/Resources/go-mtpfs-LICENSE.txt"
	-cp "$(LIBUSB_DIR)/../COPYING" "$(APP)/Contents/Resources/libusb-COPYING.txt"
	cp "$(LIBUSB_DIR)/libusb-1.0.0.dylib" "$(APP)/Contents/Frameworks/libusb-1.0.0.dylib"
	chmod u+w "$(APP)/Contents/Frameworks/libusb-1.0.0.dylib"
	install_name_tool -id @rpath/libusb-1.0.0.dylib "$(APP)/Contents/Frameworks/libusb-1.0.0.dylib"
	install_name_tool -change \
		"$$(otool -L "$(APP)/Contents/MacOS/Shioriko" | awk '/libusb-1\.0/ {print $$1; exit}')" \
		@executable_path/../Frameworks/libusb-1.0.0.dylib "$(APP)/Contents/MacOS/Shioriko"
	codesign --force $(SIGN_FLAGS) --sign "$(SIGN_IDENTITY)" "$(APP)/Contents/Frameworks/libusb-1.0.0.dylib"
	codesign --force $(SIGN_FLAGS) --sign "$(SIGN_IDENTITY)" --identifier com.love-live.jp.shioriko "$(APP)"
	@echo "Built $(APP)"

# Zip the app for copying to another Mac.
dist: app
	cd build && ditto -c -k --keepParent Shioriko.app Shioriko-$(VERSION).zip
	@echo "Built build/Shioriko-$(VERSION).zip"

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

# Run the menu bar app from the terminal, logging here as well.
run: build
	./bin/$(BIN) -v

run-headless: build
	./bin/$(BIN) -headless -v

# Regenerate packaging/AppIcon.icns from scripts/make-icon.swift.
icon:
	swift scripts/make-icon.swift packaging/AppIcon.icns

clean:
	rm -rf bin build

install: all
	mkdir -p "$(APPDIR)"
	rm -rf "$(APPDIR)/Shioriko.app"
	ditto "$(APP)" "$(APPDIR)/Shioriko.app"
	mkdir -p "$(BINDIR)"
	install -m 0755 bin/$(BIN) "$(BINDIR)/$(BIN)"
	install -m 0755 bin/$(PROBE) "$(BINDIR)/$(PROBE)"
	@echo "Installed $(APPDIR)/Shioriko.app and command-line tools in $(BINDIR)."
	@echo "Start it with: open \"$(APPDIR)/Shioriko.app\""

uninstall:
	rm -rf "$(APPDIR)/Shioriko.app"
	rm -f "$(BINDIR)/$(BIN)" "$(BINDIR)/$(PROBE)"

# Headless alternative to the app's "Launch at login": a per-user LaunchAgent
# that runs "shioriko -headless" at login. Do not use both at once.
autostart-install: build probe
	mkdir -p "$(BINDIR)"
	install -m 0755 bin/$(BIN) "$(BINDIR)/$(BIN)"
	./scripts/install-agent.sh "$(BINDIR)/$(BIN)"

autostart-uninstall:
	./scripts/uninstall-agent.sh
