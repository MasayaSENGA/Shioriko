// AppKit half of Shioriko's menu bar UI. Go pushes JSON view snapshots
// (view.go) through shkSetStatus; this file lays them out as a status item
// and its menu, and forwards menu commands back to Go.

#import <Cocoa/Cocoa.h>
#import <ServiceManagement/ServiceManagement.h>
#import <UserNotifications/UserNotifications.h>

#include "menubar.h"
#include "_cgo_export.h"

static NSString *const kPrefRevealOnMount = @"RevealOnMount";
static NSString *const kPrefNotifications = @"ShowNotifications";

// Geometry of the custom rows at the top of the menu.
static const CGFloat kMenuWidth = 320;
static const CGFloat kInset = 14;     // left/right padding
static const CGFloat kBadgeSize = 28;
static const CGFloat kTextX = kInset + kBadgeSize + 10;

static NSString *SHKString(const char *s) {
    return s ? ([NSString stringWithUTF8String:s] ?: @"") : @"";
}

static NSTextField *SHKLabel(NSFont *font, NSColor *color, NSInteger lines) {
    NSTextField *f = [NSTextField labelWithString:@""];
    f.font = font;
    f.textColor = color;
    f.maximumNumberOfLines = lines;
    f.lineBreakMode = lines == 1 ? NSLineBreakByTruncatingTail : NSLineBreakByWordWrapping;
    f.cell.truncatesLastVisibleLine = YES;
    return f;
}

// SHKLabelHeight is the height a label needs to show its text at width.
static CGFloat SHKLabelHeight(NSTextField *f, CGFloat width) {
    if (f.stringValue.length == 0) {
        return 0;
    }
    NSSize s = [f.cell cellSizeForBounds:NSMakeRect(0, 0, width, CGFLOAT_MAX)];
    return ceil(s.height);
}

// SHKSymbolFor maps a view phase to the phone glyph that represents it.
static NSString *SHKSymbolFor(NSString *phase, BOOL active) {
    if ([phase isEqualToString:@"mounted"]) {
        return active ? @"iphone.gen3.radiowaves.left.and.right" : @"iphone.gen3";
    }
    if ([phase isEqualToString:@"connecting"] || [phase isEqualToString:@"ejecting"] ||
        [phase isEqualToString:@"starting"] || [phase isEqualToString:@"quitting"]) {
        return @"iphone.gen3.radiowaves.left.and.right";
    }
    if ([phase isEqualToString:@"busy"] || [phase isEqualToString:@"failed"]) {
        return @"iphone.gen3.badge.exclamationmark";
    }
    if ([phase isEqualToString:@"paused"]) {
        return @"iphone.gen3.slash";
    }
    return @"iphone.gen3";
}

static NSImage *SHKSymbol(NSString *name, NSString *description) {
    NSImage *img = [NSImage imageWithSystemSymbolName:name accessibilityDescription:description];
    if (!img) {
        img = [NSImage imageWithSystemSymbolName:@"iphone" accessibilityDescription:description];
    }
    return img;
}

#pragma mark - Views

// SHKFlippedView lays out top-down.
@interface SHKFlippedView : NSView
@end
@implementation SHKFlippedView
- (BOOL)isFlipped {
    return YES;
}
@end

// SHKBadge is the round icon at the left of the menu header.
@interface SHKBadge : NSView
@property(nonatomic, copy) NSString *tone;
@property(nonatomic, strong) NSImageView *glyph;
@end

@implementation SHKBadge
- (instancetype)initWithFrame:(NSRect)frame {
    if ((self = [super initWithFrame:frame])) {
        _tone = @"idle";
        _glyph = [[NSImageView alloc] initWithFrame:self.bounds];
        _glyph.imageScaling = NSImageScaleProportionallyDown;
        _glyph.symbolConfiguration =
            [NSImageSymbolConfiguration configurationWithPointSize:14 weight:NSFontWeightMedium];
        [self addSubview:_glyph];
        [self applyTone];
    }
    return self;
}

- (void)setSymbol:(NSString *)name {
    self.glyph.image = SHKSymbol(name, nil);
}

- (void)setTone:(NSString *)tone {
    _tone = [tone copy];
    [self applyTone];
    [self setNeedsDisplay:YES];
}

- (void)applyTone {
    BOOL filled = [_tone isEqualToString:@"active"] || [_tone isEqualToString:@"warning"];
    self.glyph.contentTintColor = filled ? [NSColor whiteColor] : [NSColor secondaryLabelColor];
}

- (void)drawRect:(NSRect)dirty {
    NSColor *fill;
    if ([_tone isEqualToString:@"active"]) {
        fill = [NSColor controlAccentColor];
    } else if ([_tone isEqualToString:@"warning"]) {
        fill = [NSColor systemOrangeColor];
    } else {
        fill = [[NSColor labelColor] colorWithAlphaComponent:0.1];
    }
    [fill setFill];
    [[NSBezierPath bezierPathWithOvalInRect:self.bounds] fill];
}
@end

// SHKHeaderView shows the badge, a bold title, and a secondary subtitle.
@interface SHKHeaderView : SHKFlippedView
@property(nonatomic, strong) SHKBadge *badge;
@property(nonatomic, strong) NSTextField *title;
@property(nonatomic, strong) NSTextField *subtitle;
@end

@implementation SHKHeaderView
- (instancetype)init {
    if ((self = [super initWithFrame:NSMakeRect(0, 0, kMenuWidth, 48)])) {
        self.autoresizingMask = NSViewWidthSizable;
        _badge = [[SHKBadge alloc] initWithFrame:NSMakeRect(kInset, 0, kBadgeSize, kBadgeSize)];
        _title = SHKLabel([NSFont boldSystemFontOfSize:13], [NSColor labelColor], 2);
        _subtitle = SHKLabel([NSFont systemFontOfSize:11], [NSColor secondaryLabelColor], 3);
        [self addSubview:_badge];
        [self addSubview:_title];
        [self addSubview:_subtitle];
    }
    return self;
}

- (void)setTitle:(NSString *)title subtitle:(NSString *)subtitle {
    self.title.stringValue = title ?: @"";
    self.subtitle.stringValue = subtitle ?: @"";
    [self relayout];
}

- (void)setFrameSize:(NSSize)size {
    [super setFrameSize:size];
    [self relayout];
}

// relayout positions the labels and resizes the view to fit them.
- (void)relayout {
    CGFloat width = NSWidth(self.frame) - kTextX - kInset;
    CGFloat th = SHKLabelHeight(self.title, width);
    CGFloat sh = SHKLabelHeight(self.subtitle, width);
    CGFloat gap = (th > 0 && sh > 0) ? 1 : 0;
    CGFloat textHeight = th + gap + sh;
    CGFloat height = MAX(kBadgeSize, textHeight) + 16;
    if (fabs(NSHeight(self.frame) - height) > 0.5) {
        [super setFrameSize:NSMakeSize(NSWidth(self.frame), height)];
    }
    CGFloat top = floor((height - textHeight) / 2);
    self.title.frame = NSMakeRect(kTextX, top, width, th);
    self.subtitle.frame = NSMakeRect(kTextX, top + th + gap, width, sh);
    self.badge.frame = NSMakeRect(kInset, floor((height - kBadgeSize) / 2), kBadgeSize, kBadgeSize);
}
@end

// SHKUsageBar draws a storage usage bar.
@interface SHKUsageBar : NSView
@property(nonatomic) double fraction;
@property(nonatomic) BOOL warn;
@end

@implementation SHKUsageBar
- (void)setFraction:(double)fraction {
    _fraction = MAX(0, MIN(1, fraction));
    [self setNeedsDisplay:YES];
}

- (void)setWarn:(BOOL)warn {
    _warn = warn;
    [self setNeedsDisplay:YES];
}

- (void)drawRect:(NSRect)dirty {
    NSRect r = self.bounds;
    CGFloat radius = NSHeight(r) / 2;
    [[[NSColor labelColor] colorWithAlphaComponent:0.12] setFill];
    [[NSBezierPath bezierPathWithRoundedRect:r xRadius:radius yRadius:radius] fill];
    if (_fraction <= 0) {
        return;
    }
    NSRect used = r;
    used.size.width = MAX(NSHeight(r), round(NSWidth(r) * _fraction));
    [(_warn ? [NSColor systemOrangeColor] : [NSColor controlAccentColor]) setFill];
    [[NSBezierPath bezierPathWithRoundedRect:used xRadius:radius yRadius:radius] fill];
}
@end

// SHKStorageView shows one storage: its name, free space, and a usage bar.
@interface SHKStorageView : SHKFlippedView
@property(nonatomic, strong) NSTextField *name;
@property(nonatomic, strong) NSTextField *detail;
@property(nonatomic, strong) SHKUsageBar *bar;
@end

@implementation SHKStorageView
- (instancetype)initWithStorage:(NSDictionary *)s {
    if ((self = [super initWithFrame:NSMakeRect(0, 0, kMenuWidth, 38)])) {
        self.autoresizingMask = NSViewWidthSizable;
        _name = SHKLabel([NSFont systemFontOfSize:12], [NSColor labelColor], 1);
        _detail = SHKLabel([NSFont monospacedDigitSystemFontOfSize:11 weight:NSFontWeightRegular],
                           [NSColor secondaryLabelColor], 1);
        _detail.alignment = NSTextAlignmentRight;
        _bar = [[SHKUsageBar alloc] initWithFrame:NSZeroRect];
        [self addSubview:_name];
        [self addSubview:_detail];
        [self addSubview:_bar];
        [self update:s];
    }
    return self;
}

- (void)update:(NSDictionary *)s {
    self.name.stringValue = [s[@"name"] isKindOfClass:NSString.class] ? s[@"name"] : @"";
    self.detail.stringValue = [s[@"detail"] isKindOfClass:NSString.class] ? s[@"detail"] : @"";
    self.bar.fraction = [s[@"fraction"] doubleValue];
    self.bar.warn = [s[@"warn"] boolValue];
    self.toolTip = [NSString stringWithFormat:@"%@ — %@", self.name.stringValue, self.detail.stringValue];
    [self relayout];
}

- (void)setFrameSize:(NSSize)size {
    [super setFrameSize:size];
    [self relayout];
}

- (void)relayout {
    // The free-space figure gets the room it needs; the name truncates.
    CGFloat width = NSWidth(self.frame) - kTextX - kInset;
    CGFloat detailWidth = MIN(width * 0.75, ceil([self.detail.cell cellSize].width) + 2);
    self.detail.frame = NSMakeRect(kTextX + width - detailWidth, 4, detailWidth, 16);
    self.name.frame = NSMakeRect(kTextX, 4, MAX(0, width - detailWidth - 8), 16);
    self.bar.frame = NSMakeRect(kTextX, 24, width, 6);
}
@end

// SHKNoteView is a small secondary line (transfer speed, hints).
@interface SHKNoteView : SHKFlippedView
@property(nonatomic, strong) NSTextField *label;
@end

@implementation SHKNoteView
- (instancetype)initWithText:(NSString *)text {
    if ((self = [super initWithFrame:NSMakeRect(0, 0, kMenuWidth, 20)])) {
        self.autoresizingMask = NSViewWidthSizable;
        _label = SHKLabel([NSFont monospacedDigitSystemFontOfSize:11 weight:NSFontWeightRegular],
                          [NSColor secondaryLabelColor], 3);
        _label.stringValue = text ?: @"";
        [self addSubview:_label];
        [self relayout];
    }
    return self;
}

- (void)setFrameSize:(NSSize)size {
    [super setFrameSize:size];
    [self relayout];
}

- (void)relayout {
    CGFloat width = NSWidth(self.frame) - kTextX - kInset;
    CGFloat h = SHKLabelHeight(self.label, width);
    CGFloat height = h + 6;
    if (fabs(NSHeight(self.frame) - height) > 0.5) {
        [super setFrameSize:NSMakeSize(NSWidth(self.frame), height)];
    }
    self.label.frame = NSMakeRect(kTextX, 2, width, h);
}
@end

#pragma mark - Application

@interface SHKApp : NSObject <NSApplicationDelegate, NSMenuDelegate, UNUserNotificationCenterDelegate>
@property(nonatomic, copy) NSString *version;
@property(nonatomic, copy) NSString *logPath;
@property(nonatomic, strong) NSDictionary *status;
@property(nonatomic) BOOL quitRequested;
@property(nonatomic) BOOL cleanupDone;
@end

static SHKApp *gApp;

@implementation SHKApp {
    NSStatusItem *_statusItem;
    NSMenu *_menu;
    SHKHeaderView *_header;
    NSMutableArray<NSMenuItem *> *_detailItems;
    NSString *_detailSignature;
    NSMenuItem *_revealItem, *_ejectItem, *_mountItem, *_actionsSeparator;
    NSMenuItem *_pauseItem, *_revealPrefItem, *_notifyPrefItem, *_loginItem;
}

- (BOOL)isBundledApp {
    NSBundle *b = NSBundle.mainBundle;
    return b.bundleIdentifier.length > 0 && [b.bundleURL.pathExtension isEqualToString:@"app"];
}

- (void)activate {
    if (@available(macOS 14, *)) {
        [NSApp activate];
    } else {
        [NSApp activateIgnoringOtherApps:YES];
    }
}

- (NSMenuItem *)itemWithTitle:(NSString *)title action:(SEL)action key:(NSString *)key symbol:(NSString *)symbol {
    NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:title action:action keyEquivalent:key ?: @""];
    item.target = self;
    if (symbol) {
        item.image = SHKSymbol(symbol, nil);
    }
    return item;
}

- (void)applicationWillFinishLaunching:(NSNotification *)note {
    if ([self isBundledApp]) {
        UNUserNotificationCenter.currentNotificationCenter.delegate = self;
    }
}

- (void)applicationDidFinishLaunching:(NSNotification *)note {
    _detailItems = [NSMutableArray array];
    _menu = [[NSMenu alloc] initWithTitle:@"Shioriko"];
    _menu.delegate = self;
    _menu.autoenablesItems = NO;
    _menu.minimumWidth = kMenuWidth;

    _header = [[SHKHeaderView alloc] init];
    NSMenuItem *headerItem = [[NSMenuItem alloc] init];
    headerItem.view = _header;
    [_menu addItem:headerItem];

    _actionsSeparator = [NSMenuItem separatorItem];
    [_menu addItem:_actionsSeparator];
    _revealItem = [self itemWithTitle:@"Finderで開く" action:@selector(reveal:) key:@"o" symbol:@"folder"];
    _ejectItem = [self itemWithTitle:@"取り出す" action:@selector(eject:) key:@"e" symbol:@"eject"];
    _mountItem = [self itemWithTitle:@"マウント" action:@selector(mount:) key:@"" symbol:@"arrow.clockwise"];
    [_menu addItem:_revealItem];
    [_menu addItem:_ejectItem];
    [_menu addItem:_mountItem];

    [_menu addItem:[NSMenuItem separatorItem]];
    _pauseItem = [self itemWithTitle:@"自動マウントを一時停止" action:@selector(togglePause:) key:@"" symbol:@"pause.circle"];
    [_menu addItem:_pauseItem];

    [_menu addItem:[NSMenuItem separatorItem]];
    NSMenu *settings = [[NSMenu alloc] initWithTitle:@"設定"];
    settings.autoenablesItems = NO;
    _revealPrefItem = [self itemWithTitle:@"マウントしたらFinderで開く" action:@selector(toggleRevealPref:) key:@"" symbol:nil];
    _notifyPrefItem = [self itemWithTitle:@"接続と取り外しを通知" action:@selector(toggleNotifyPref:) key:@"" symbol:nil];
    _loginItem = [self itemWithTitle:@"ログイン時に起動" action:@selector(toggleLoginItem:) key:@"" symbol:nil];
    [settings addItem:_revealPrefItem];
    [settings addItem:_notifyPrefItem];
    [settings addItem:[NSMenuItem separatorItem]];
    [settings addItem:_loginItem];
    NSMenuItem *settingsItem = [self itemWithTitle:@"設定" action:nil key:@"" symbol:@"gearshape"];
    settingsItem.submenu = settings;
    [_menu addItem:settingsItem];
    [_menu addItem:[self itemWithTitle:@"ログを表示…" action:@selector(openLog:) key:@"" symbol:@"doc.text"]];

    [_menu addItem:[NSMenuItem separatorItem]];
    [_menu addItem:[self itemWithTitle:@"Shiorikoについて" action:@selector(about:) key:@"" symbol:@"info.circle"]];
    NSMenuItem *quit = [[NSMenuItem alloc] initWithTitle:@"Shiorikoを終了" action:@selector(terminate:) keyEquivalent:@"q"];
    quit.target = NSApp;
    quit.image = SHKSymbol(@"power", nil);
    [_menu addItem:quit];

    _statusItem = [NSStatusBar.systemStatusBar statusItemWithLength:NSVariableStatusItemLength];
    _statusItem.autosaveName = @"Shioriko";
    _statusItem.menu = _menu;
    _statusItem.button.imagePosition = NSImageOnly;

    [self refresh];

    if ([self isBundledApp] && [NSUserDefaults.standardUserDefaults boolForKey:kPrefNotifications]) {
        [self requestNotificationAuthorization];
    }
}

- (NSApplicationTerminateReply)applicationShouldTerminate:(NSApplication *)sender {
    if (self.cleanupDone) {
        return NSTerminateNow;
    }
    if (!self.quitRequested) {
        self.quitRequested = YES;
        shkGoQuit();
    }
    return NSTerminateLater;
}

#pragma mark Status

- (void)applyStatus:(NSDictionary *)status {
    self.status = status;
    [self refresh];
}

static BOOL SHKBool(NSDictionary *d, NSString *key) {
    id v = d[key];
    return [v respondsToSelector:@selector(boolValue)] && [v boolValue];
}

static NSString *SHKStr(NSDictionary *d, NSString *key) {
    id v = d[key];
    return [v isKindOfClass:NSString.class] ? v : @"";
}

static NSArray *SHKArr(NSDictionary *d, NSString *key) {
    id v = d[key];
    return [v isKindOfClass:NSArray.class] ? v : @[];
}

- (void)refresh {
    if (!_statusItem) {
        return;
    }
    NSDictionary *s = self.status ?: @{};
    NSString *phase = SHKStr(s, @"phase");
    BOOL active = SHKBool(s, @"active");

    // Status item icon: dimmed when there is nothing mounted and nothing wrong.
    NSImage *icon = SHKSymbol(SHKSymbolFor(phase, active), @"Shioriko");
    icon.template = YES;
    _statusItem.button.image = icon;
    _statusItem.button.appearsDisabled =
        [phase isEqualToString:@"waiting"] || [phase isEqualToString:@"ejected"] ||
        [phase isEqualToString:@"paused"] || [phase isEqualToString:@"stopped"];
    NSString *tooltip = SHKStr(s, @"tooltip");
    _statusItem.button.toolTip = tooltip.length ? tooltip : @"Shioriko";

    // Header.
    NSString *tone = SHKStr(s, @"tone");
    _header.badge.tone = tone.length ? tone : @"idle";
    [_header.badge setSymbol:SHKSymbolFor(phase, NO)];
    [_header setTitle:SHKStr(s, @"title") subtitle:SHKStr(s, @"subtitle")];

    [self refreshDetails:s];

    // Actions.
    _revealItem.hidden = !SHKBool(s, @"canReveal");
    _ejectItem.hidden = !SHKBool(s, @"canEject") && ![phase isEqualToString:@"ejecting"];
    _ejectItem.enabled = SHKBool(s, @"canEject");
    NSString *ejectTitle = SHKStr(s, @"ejectTitle");
    _ejectItem.title = ejectTitle.length ? ejectTitle : @"取り出す";
    _mountItem.hidden = !SHKBool(s, @"canMount");
    NSString *mountTitle = SHKStr(s, @"mountTitle");
    _mountItem.title = mountTitle.length ? mountTitle : @"マウント";
    _actionsSeparator.hidden = _revealItem.hidden && _ejectItem.hidden && _mountItem.hidden;

    BOOL paused = SHKBool(s, @"paused");
    _pauseItem.title = paused ? @"自動マウントを再開" : @"自動マウントを一時停止";
    _pauseItem.image = SHKSymbol(paused ? @"play.circle" : @"pause.circle", nil);
    _pauseItem.enabled = SHKBool(s, @"canPause");

    [self refreshSettings];
}

// refreshDetails rebuilds the storage and note rows under the header. Rows are
// only recreated when their number changes, so an open menu does not flicker.
- (void)refreshDetails:(NSDictionary *)s {
    NSArray *storages = SHKArr(s, @"storages");
    NSArray *notes = SHKArr(s, @"notes");
    NSString *signature = [NSString stringWithFormat:@"%lu/%lu", (unsigned long)storages.count, (unsigned long)notes.count];

    if ([signature isEqualToString:_detailSignature]) {
        NSUInteger i = 0;
        for (NSDictionary *st in storages) {
            [(SHKStorageView *)_detailItems[i++].view update:st];
        }
        for (NSString *n in notes) {
            SHKNoteView *v = (SHKNoteView *)_detailItems[i++].view;
            v.label.stringValue = [n isKindOfClass:NSString.class] ? n : @"";
            [v relayout];
        }
        return;
    }

    for (NSMenuItem *item in _detailItems) {
        [_menu removeItem:item];
    }
    [_detailItems removeAllObjects];
    NSInteger index = 1; // right after the header
    for (NSDictionary *st in storages) {
        if (![st isKindOfClass:NSDictionary.class]) {
            continue;
        }
        NSMenuItem *item = [[NSMenuItem alloc] init];
        item.view = [[SHKStorageView alloc] initWithStorage:st];
        [_menu insertItem:item atIndex:index++];
        [_detailItems addObject:item];
    }
    for (NSString *n in notes) {
        if (![n isKindOfClass:NSString.class]) {
            continue;
        }
        NSMenuItem *item = [[NSMenuItem alloc] init];
        item.view = [[SHKNoteView alloc] initWithText:n];
        [_menu insertItem:item atIndex:index++];
        [_detailItems addObject:item];
    }
    _detailSignature = signature;
}

- (void)refreshSettings {
    NSUserDefaults *d = NSUserDefaults.standardUserDefaults;
    _revealPrefItem.state = [d boolForKey:kPrefRevealOnMount] ? NSControlStateValueOn : NSControlStateValueOff;
    _notifyPrefItem.state = [d boolForKey:kPrefNotifications] ? NSControlStateValueOn : NSControlStateValueOff;
    _notifyPrefItem.hidden = ![self isBundledApp];

    if (![self isBundledApp]) {
        _loginItem.hidden = YES;
        return;
    }
    switch (SMAppService.mainAppService.status) {
    case SMAppServiceStatusEnabled:
        _loginItem.state = NSControlStateValueOn;
        _loginItem.toolTip = nil;
        break;
    case SMAppServiceStatusRequiresApproval:
        _loginItem.state = NSControlStateValueMixed;
        _loginItem.toolTip = @"システム設定の「ログイン項目」で許可が必要です";
        break;
    default:
        _loginItem.state = NSControlStateValueOff;
        _loginItem.toolTip = nil;
        break;
    }
}

- (void)menuWillOpen:(NSMenu *)menu {
    [self refreshSettings];
}

#pragma mark Actions

- (void)reveal:(id)sender {
    NSString *mp = SHKStr(self.status ?: @{}, @"mountPoint");
    if (mp.length) {
        [NSWorkspace.sharedWorkspace openURL:[NSURL fileURLWithPath:mp isDirectory:YES]];
    }
}

- (void)eject:(id)sender {
    shkGoAction(SHK_ACTION_EJECT);
}

- (void)mount:(id)sender {
    shkGoAction(SHK_ACTION_MOUNT);
}

- (void)togglePause:(id)sender {
    shkGoAction(SHK_ACTION_TOGGLE_PAUSE);
}

- (void)toggleRevealPref:(id)sender {
    NSUserDefaults *d = NSUserDefaults.standardUserDefaults;
    [d setBool:![d boolForKey:kPrefRevealOnMount] forKey:kPrefRevealOnMount];
    [self refreshSettings];
}

- (void)toggleNotifyPref:(id)sender {
    NSUserDefaults *d = NSUserDefaults.standardUserDefaults;
    BOOL on = ![d boolForKey:kPrefNotifications];
    [d setBool:on forKey:kPrefNotifications];
    if (on) {
        [self requestNotificationAuthorization];
    }
    [self refreshSettings];
}

- (void)toggleLoginItem:(id)sender {
    SMAppService *service = SMAppService.mainAppService;
    if (service.status == SMAppServiceStatusRequiresApproval) {
        [SMAppService openSystemSettingsLoginItems];
        return;
    }
    NSError *err = nil;
    BOOL ok = service.status == SMAppServiceStatusEnabled ? [service unregisterAndReturnError:&err]
                                                          : [service registerAndReturnError:&err];
    [self refreshSettings];
    if (!ok) {
        [self showAlert:@"ログイン項目を変更できませんでした" message:err.localizedDescription ?: @""];
    } else if (service.status == SMAppServiceStatusRequiresApproval) {
        [self showAlert:@"ログイン項目の許可が必要です"
                message:@"システム設定の「一般」>「ログイン項目と機能拡張」で Shioriko を許可してください。"];
        [SMAppService openSystemSettingsLoginItems];
    }
}

- (void)openLog:(id)sender {
    if (self.logPath.length == 0) {
        return;
    }
    NSURL *url = [NSURL fileURLWithPath:self.logPath];
    if (![NSFileManager.defaultManager fileExistsAtPath:self.logPath]) {
        [self showAlert:@"ログファイルがありません" message:self.logPath];
        return;
    }
    [NSWorkspace.sharedWorkspace openURL:url];
}

- (void)about:(id)sender {
    [self activate];
    NSMutableParagraphStyle *para = [[NSMutableParagraphStyle alloc] init];
    para.alignment = NSTextAlignmentCenter;
    NSAttributedString *credits = [[NSAttributedString alloc]
        initWithString:@"Android スマートフォン（MTP）を Finder のボリュームとしてマウントします。\n\n"
                       @"go-mtpfs（New BSD ライセンス）と libusb（LGPL-2.1）を使用しています。"
            attributes:@{
                NSFontAttributeName : [NSFont systemFontOfSize:[NSFont smallSystemFontSize]],
                NSForegroundColorAttributeName : [NSColor secondaryLabelColor],
                NSParagraphStyleAttributeName : para,
            }];
    NSMutableDictionary *opts = [@{NSAboutPanelOptionCredits : credits} mutableCopy];
    if (![self isBundledApp]) {
        opts[NSAboutPanelOptionApplicationName] = @"Shioriko";
        opts[NSAboutPanelOptionApplicationVersion] = self.version ?: @"";
    }
    [NSApp orderFrontStandardAboutPanelWithOptions:opts];
}

- (void)showAlert:(NSString *)title message:(NSString *)message {
    NSAlert *a = [[NSAlert alloc] init];
    a.messageText = title;
    a.informativeText = message;
    [self activate];
    [a runModal];
}

// runAlert shows an alert requested by Go (see shkAlert) and reports the
// answer. It must not run inside a main-queue block: the main queue is serial,
// so while the modal loop ran no other block (status updates, the terminate
// reply) could execute. shkAlert therefore schedules it with
// performSelectorOnMainThread, which goes through the run loop instead.
- (void)runAlert:(NSDictionary *)info {
    NSAlert *a = [[NSAlert alloc] init];
    a.alertStyle = NSAlertStyleWarning;
    a.messageText = info[@"title"];
    a.informativeText = info[@"message"];
    [a addButtonWithTitle:info[@"button1"]];
    NSString *b2 = info[@"button2"];
    if (b2.length) {
        NSButton *second = [a addButtonWithTitle:b2];
        second.hasDestructiveAction = [info[@"destructive"] boolValue];
    }
    [self activate];
    NSModalResponse r = [a runModal];
    int button = r == NSAlertFirstButtonReturn ? 1 : (r == NSAlertSecondButtonReturn ? 2 : 0);
    shkGoAlertResult([info[@"tag"] intValue], button);
}

#pragma mark Notifications

- (void)requestNotificationAuthorization {
    if (![self isBundledApp]) {
        return;
    }
    [UNUserNotificationCenter.currentNotificationCenter
        requestAuthorizationWithOptions:UNAuthorizationOptionAlert | UNAuthorizationOptionSound
                      completionHandler:^(BOOL granted, NSError *error) {
                        if (error) {
                            NSLog(@"shioriko: notification authorization: %@", error);
                        }
                      }];
}

- (void)notify:(NSString *)title body:(NSString *)body path:(NSString *)path {
    if (![self isBundledApp] || ![NSUserDefaults.standardUserDefaults boolForKey:kPrefNotifications]) {
        return;
    }
    UNMutableNotificationContent *content = [[UNMutableNotificationContent alloc] init];
    content.title = title;
    content.body = body;
    if (path.length) {
        content.userInfo = @{@"path" : path};
    }
    UNNotificationRequest *req = [UNNotificationRequest requestWithIdentifier:NSUUID.UUID.UUIDString
                                                                      content:content
                                                                      trigger:nil];
    [UNUserNotificationCenter.currentNotificationCenter addNotificationRequest:req withCompletionHandler:nil];
}

- (void)userNotificationCenter:(UNUserNotificationCenter *)center
       willPresentNotification:(UNNotification *)notification
         withCompletionHandler:(void (^)(UNNotificationPresentationOptions))completionHandler {
    completionHandler(UNNotificationPresentationOptionBanner | UNNotificationPresentationOptionList);
}

- (void)userNotificationCenter:(UNUserNotificationCenter *)center
    didReceiveNotificationResponse:(UNNotificationResponse *)response
             withCompletionHandler:(void (^)(void))completionHandler {
    NSString *path = response.notification.request.content.userInfo[@"path"];
    if ([path isKindOfClass:NSString.class] && [NSFileManager.defaultManager fileExistsAtPath:path]) {
        [NSWorkspace.sharedWorkspace openURL:[NSURL fileURLWithPath:path isDirectory:YES]];
    }
    completionHandler();
}
@end

#pragma mark - C entry points

void shkInit(const char *version, const char *logPath) {
    @autoreleasepool {
        [NSUserDefaults.standardUserDefaults registerDefaults:@{
            kPrefRevealOnMount : @YES,
            kPrefNotifications : @YES,
        }];
        [NSApplication sharedApplication];
        gApp = [[SHKApp alloc] init];
        gApp.version = SHKString(version);
        gApp.logPath = SHKString(logPath);
        NSApp.delegate = gApp;
        // No Dock icon or app menu, even when not launched from the bundle.
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
    }
}

void shkRun(void) {
    @autoreleasepool {
        [NSApp run];
    }
}

void shkSetStatus(const char *json) {
    @autoreleasepool {
        NSData *data = [NSData dataWithBytes:json length:strlen(json)];
        dispatch_async(dispatch_get_main_queue(), ^{
          id obj = [NSJSONSerialization JSONObjectWithData:data options:0 error:nil];
          if ([obj isKindOfClass:NSDictionary.class]) {
              [gApp applyStatus:obj];
          }
        });
    }
}

void shkNotify(const char *title, const char *body, const char *path) {
    @autoreleasepool {
        NSString *t = SHKString(title), *b = SHKString(body), *p = SHKString(path);
        dispatch_async(dispatch_get_main_queue(), ^{
          [gApp notify:t body:b path:p];
        });
    }
}

void shkAlert(int tag, const char *title, const char *message, const char *button1, const char *button2,
              int button2Destructive) {
    @autoreleasepool {
        NSDictionary *info = @{
            @"tag" : @(tag),
            @"title" : SHKString(title),
            @"message" : SHKString(message),
            @"button1" : SHKString(button1),
            @"button2" : button2 ? SHKString(button2) : @"",
            @"destructive" : @(button2Destructive != 0),
        };
        [gApp performSelectorOnMainThread:@selector(runAlert:)
                               withObject:info
                            waitUntilDone:NO
                                    modes:@[ NSRunLoopCommonModes ]];
    }
}

void shkTerminateReply(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
      gApp.cleanupDone = YES;
      if (gApp.quitRequested) {
          [NSApp replyToApplicationShouldTerminate:YES];
      } else {
          [NSApp terminate:nil];
      }
    });
}

int shkPrefBool(const char *key, int def) {
    @autoreleasepool {
        NSString *k = SHKString(key);
        id v = [NSUserDefaults.standardUserDefaults objectForKey:k];
        if (![v respondsToSelector:@selector(boolValue)]) {
            return def;
        }
        return [v boolValue] ? 1 : 0;
    }
}
