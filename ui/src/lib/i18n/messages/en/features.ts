/**
 * English entries of the feature modules: the file browsers, the sharing
 * dialogs, the upload pipeline and the login flow.
 *
 * Keys are `features.<module>.<name>`.
 */
export const features = {
  // The file browser shell (`features/files/file-browser.tsx`).
  "features.fileBrowser.upOneFolder": "Up one folder",
  "features.fileBrowser.listView": "List view",
  "features.fileBrowser.gridView": "Grid view",
  "features.fileBrowser.filesAndFolders": "Files and folders",
  "features.fileBrowser.currentFolder": "Current folder",
  "features.fileBrowser.folderKind": "Folder",
  "features.fileBrowser.selectFile": "Select {name}",
  "features.fileBrowser.emptyTitle": "This folder is empty",
  "features.fileBrowser.emptyHint": "This folder is empty.",
  "features.fileBrowser.emptyShared": "Files and folders you shared appear here.",
  "features.fileBrowser.emptySharedWithMe": "Files and folders shared with you appear here.",
  "features.fileBrowser.emptySharedFolder": "This shared folder is empty.",
  "features.fileBrowser.selected": "{count} selected",
  "features.fileBrowser.root.shared": "Shared",
  "features.fileBrowser.root.sharedWithMe": "Shared with me",

  // Selection toolbar and dialogs of the shared browsers.
  "features.fileBrowser.action.newFolder": "New folder",
  "features.fileBrowser.action.uploadFiles": "Upload files",
  "features.fileBrowser.action.chooseFiles": "Choose upload files",
  "features.fileBrowser.action.rename": "Rename selected item",
  "features.fileBrowser.action.share": "Share selected item",
  "features.fileBrowser.action.download": "Download selected file",
  "features.fileBrowser.action.stopSharing": "Stop sharing selected items",
  "features.fileBrowser.action.trash": "Move selected items to trash",
  "features.fileBrowser.action.clearSelection": "Clear selection",
  "features.fileBrowser.dialog.createFolderTitle": "Create folder",
  "features.fileBrowser.dialog.renameTitle": "Rename item",

  "features.fileBrowser.toast.folderCreated": "Folder created",
  "features.fileBrowser.toast.folderCreateFailed": "Folder could not be created",
  "features.fileBrowser.toast.renamed": "Item renamed",
  "features.fileBrowser.toast.renameFailed": "Item could not be renamed",
  "features.fileBrowser.toast.trashed": {
    one: "{count} item moved to trash",
    other: "{count} items moved to trash",
  },
  "features.fileBrowser.toast.trashFailed": "Items could not be moved to trash",
  "features.fileBrowser.toast.shareRemoveFailed": "Sharing could not be removed",
  "features.fileBrowser.toast.unshared": {
    one: "{count} item no longer shared",
    other: "{count} items no longer shared",
  },

  // Destination picker for move and copy (`features/files/folder-picker.tsx`).
  "features.folderPicker.destination": "Destination folder",
  "features.folderPicker.driveRoot": "Drive root",
  "features.folderPicker.folders": "Folders",
  "features.folderPicker.loadFailed": "Folders could not be loaded.",
  "features.folderPicker.empty": "No folders here.",
  "features.folderPicker.confirm": "Move here",
  "features.folderPicker.loadMore": "Load more folders",

  // Share dialog (`features/files/share-dialog.tsx`).
  "features.shareDialog.title": "Share {name}",
  "features.shareDialog.titleFallback": "Share item",
  "features.shareDialog.description": "Manage who can access this item and create public links.",
  "features.shareDialog.peopleWithAccess": "People with access",
  "features.shareDialog.accessScopeHint": "Access applies to this item and its descendants.",
  "features.shareDialog.addUser": "Add a user",
  "features.shareDialog.searchUsers": "Search by name, username, or Telegram ID",
  "features.shareDialog.permission": "Permission",
  "features.shareDialog.expiration": "Expiration",
  "features.shareDialog.userFallback": "User {id}",
  "features.shareDialog.telegramId": "Telegram ID {id}",
  "features.shareDialog.noUsers": "No users found.",
  "features.shareDialog.noGrants": "No Teldrive users have access yet.",
  "features.shareDialog.expires": "Expires {date}",
  "features.shareDialog.noExpiration": "No expiration",
  "features.shareDialog.viewer": "Viewer",
  "features.shareDialog.editor": "Editor",
  "features.shareDialog.grantActions": "Grant actions",
  "features.shareDialog.changePermission": "Change permission",
  "features.shareDialog.makeEditor": "Make editor",
  "features.shareDialog.makeViewer": "Make viewer",
  "features.shareDialog.removeAccess": "Remove access",
  "features.shareDialog.publicLinks": "Public links",
  "features.shareDialog.publicLinksHint":
    "Anyone with a link can use the selected permission. Password protection is optional.",
  "features.shareDialog.optionalPassword": "Optional password",
  "features.shareDialog.createPublicLink": "Create public link",
  "features.shareDialog.linkCopied": "Link copied",
  "features.shareDialog.linkCopyFailed": "Link could not be copied",
  "features.shareDialog.passwordProtected": "Password protected",
  "features.shareDialog.downloadCount": {
    one: "{count} download",
    other: "{count} downloads",
  },
  "features.shareDialog.publicViewerLink": "Public viewer link",
  "features.shareDialog.publicEditorLink": "Public editor link",
  "features.shareDialog.publicLinkActions": "Public link actions",
  "features.shareDialog.revokeLink": "Revoke link",
  "features.shareDialog.noPublicLinks": "No public links exist for this item.",
  "features.shareDialog.sharePermission": "Share permission",
  "features.shareDialog.customExpiration": "Custom expiration duration",
  "features.shareDialog.customExpirationPlaceholder": "1h, 1d12h, 1y",
  "features.shareDialog.duration.oneHour": "1 hour",
  "features.shareDialog.duration.oneDay": "1 day",
  "features.shareDialog.duration.sevenDays": "7 days",
  "features.shareDialog.duration.thirtyDays": "30 days",
  "features.shareDialog.duration.custom": "Custom",
  "features.shareDialog.duration.customOption": "Custom…",

  "features.shareDialog.toast.accessGranted": "Access granted",
  "features.shareDialog.toast.accessGrantFailed": "Access could not be granted",
  "features.shareDialog.toast.permissionChangeFailed": "Permission could not be changed",
  "features.shareDialog.toast.accessRemoved": "Access removed",
  "features.shareDialog.toast.accessRemoveFailed": "Access could not be removed",
  "features.shareDialog.toast.publicLinkCreated": "Public link created and copied",
  "features.shareDialog.toast.publicLinkCreateFailed": "Public link could not be created",
  "features.shareDialog.toast.publicLinkRevoked": "Public link revoked",
  "features.shareDialog.toast.publicLinkRevokeFailed": "Public link could not be revoked",

  // Upload pipeline (`features/uploads/store.ts`). Diagnostics such as the
  // malformed-session and upload-paused messages stay in the source.
  "features.uploads.batchName": {
    one: "{count} file",
    other: "{count} files",
  },
  "features.uploads.originalFileUnavailable":
    "The original file is no longer available. Start the upload again.",
  "features.uploads.folderMissing": "The folder {path} is no longer available.",

  // Sign-in validation (`auth/login-schema.ts`).
  "features.auth.phoneFormat": "Use E.164 format, including the country code.",
  "features.auth.codeRequired": "Enter the code sent by Telegram.",
  "features.auth.passwordRequired": "Enter your Telegram two-step verification password.",

  // Form scaffolding (`forms/app-form.tsx`).
  "features.form.errorSeparator": ". ",
} as const;
