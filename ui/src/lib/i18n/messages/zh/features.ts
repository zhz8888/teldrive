/**
 * 功能模块的中文词条。
 *
 * 键与 `messages/en/features.ts` 一一对应。
 */
import type { AreaMessages } from "../../types";
import type { features as enFeatures } from "../en/features";

export const features: AreaMessages<typeof enFeatures> = {
  // 文件浏览器外壳（`features/files/file-browser.tsx`）。
  "features.fileBrowser.upOneFolder": "上一级文件夹",
  "features.fileBrowser.listView": "列表视图",
  "features.fileBrowser.gridView": "网格视图",
  "features.fileBrowser.filesAndFolders": "文件与文件夹",
  "features.fileBrowser.currentFolder": "当前文件夹",
  "features.fileBrowser.folderKind": "文件夹",
  "features.fileBrowser.selectFile": "选择 {name}",
  "features.fileBrowser.emptyTitle": "此文件夹为空",
  "features.fileBrowser.emptyHint": "此文件夹为空。",
  "features.fileBrowser.emptyShared": "你分享的文件和文件夹会显示在这里。",
  "features.fileBrowser.emptySharedWithMe": "共享给你的文件和文件夹会显示在这里。",
  "features.fileBrowser.emptySharedFolder": "此共享文件夹为空。",
  "features.fileBrowser.selected": "已选择 {count} 项",
  "features.fileBrowser.root.shared": "已分享",
  "features.fileBrowser.root.sharedWithMe": "共享给我",

  // 共享浏览器的选择工具栏与对话框。
  "features.fileBrowser.action.newFolder": "新建文件夹",
  "features.fileBrowser.action.uploadFiles": "上传文件",
  "features.fileBrowser.action.chooseFiles": "选择要上传的文件",
  "features.fileBrowser.action.rename": "重命名所选项",
  "features.fileBrowser.action.share": "分享所选项",
  "features.fileBrowser.action.download": "下载所选文件",
  "features.fileBrowser.action.stopSharing": "停止分享所选项",
  "features.fileBrowser.action.trash": "将所选项移到回收站",
  "features.fileBrowser.action.clearSelection": "清除选择",
  "features.fileBrowser.dialog.createFolderTitle": "新建文件夹",
  "features.fileBrowser.dialog.renameTitle": "重命名项目",

  "features.fileBrowser.toast.folderCreated": "文件夹已创建",
  "features.fileBrowser.toast.folderCreateFailed": "无法创建文件夹",
  "features.fileBrowser.toast.renamed": "项目已重命名",
  "features.fileBrowser.toast.renameFailed": "无法重命名项目",
  "features.fileBrowser.toast.trashed": {
    one: "已将 {count} 项移到回收站",
    other: "已将 {count} 项移到回收站",
  },
  "features.fileBrowser.toast.trashFailed": "无法将项目移到回收站",
  "features.fileBrowser.toast.shareRemoveFailed": "无法取消分享",
  "features.fileBrowser.toast.unshared": {
    one: "已取消分享 {count} 项",
    other: "已取消分享 {count} 项",
  },

  // 移动与复制的目标选择器（`features/files/folder-picker.tsx`）。
  "features.folderPicker.destination": "目标文件夹",
  "features.folderPicker.driveRoot": "云盘根目录",
  "features.folderPicker.folders": "文件夹",
  "features.folderPicker.loadFailed": "无法加载文件夹。",
  "features.folderPicker.empty": "此处没有文件夹。",
  "features.folderPicker.confirm": "移动到此",

  // 分享对话框（`features/files/share-dialog.tsx`）。
  "features.shareDialog.title": "分享 {name}",
  "features.shareDialog.titleFallback": "分享项目",
  "features.shareDialog.description": "管理谁可以访问此项目，并创建公开链接。",
  "features.shareDialog.peopleWithAccess": "可访问的用户",
  "features.shareDialog.accessScopeHint": "访问权限适用于此项目及其下级内容。",
  "features.shareDialog.addUser": "添加用户",
  "features.shareDialog.searchUsers": "按姓名、用户名或 Telegram ID 搜索",
  "features.shareDialog.permission": "权限",
  "features.shareDialog.expiration": "有效期",
  "features.shareDialog.userFallback": "用户 {id}",
  "features.shareDialog.telegramId": "Telegram ID {id}",
  "features.shareDialog.noUsers": "未找到用户。",
  "features.shareDialog.noGrants": "还没有 Teldrive 用户拥有访问权限。",
  "features.shareDialog.expires": "有效期至 {date}",
  "features.shareDialog.noExpiration": "永不过期",
  "features.shareDialog.viewer": "查看者",
  "features.shareDialog.editor": "编辑者",
  "features.shareDialog.grantActions": "授权操作",
  "features.shareDialog.changePermission": "更改权限",
  "features.shareDialog.makeEditor": "设为编辑者",
  "features.shareDialog.makeViewer": "设为查看者",
  "features.shareDialog.removeAccess": "移除访问权限",
  "features.shareDialog.publicLinks": "公开链接",
  "features.shareDialog.publicLinksHint": "任何获得链接的人都可以使用所选权限。密码保护为可选项。",
  "features.shareDialog.optionalPassword": "可选密码",
  "features.shareDialog.createPublicLink": "创建公开链接",
  "features.shareDialog.linkCopied": "链接已复制",
  "features.shareDialog.linkCopyFailed": "无法复制链接",
  "features.shareDialog.passwordProtected": "已设置密码",
  "features.shareDialog.downloadCount": {
    one: "{count} 次下载",
    other: "{count} 次下载",
  },
  "features.shareDialog.publicViewerLink": "公开查看链接",
  "features.shareDialog.publicEditorLink": "公开编辑链接",
  "features.shareDialog.publicLinkActions": "公开链接操作",
  "features.shareDialog.revokeLink": "撤销链接",
  "features.shareDialog.noPublicLinks": "此项目没有公开链接。",
  "features.shareDialog.sharePermission": "分享权限",
  "features.shareDialog.customExpiration": "自定义有效期时长",
  "features.shareDialog.customExpirationPlaceholder": "1h、1d12h、1y",
  "features.shareDialog.duration.oneHour": "1 小时",
  "features.shareDialog.duration.oneDay": "1 天",
  "features.shareDialog.duration.sevenDays": "7 天",
  "features.shareDialog.duration.thirtyDays": "30 天",
  "features.shareDialog.duration.custom": "自定义",
  "features.shareDialog.duration.customOption": "自定义…",

  "features.shareDialog.toast.accessGranted": "已授予访问权限",
  "features.shareDialog.toast.accessGrantFailed": "无法授予访问权限",
  "features.shareDialog.toast.permissionChangeFailed": "无法更改权限",
  "features.shareDialog.toast.accessRemoved": "已移除访问权限",
  "features.shareDialog.toast.accessRemoveFailed": "无法移除访问权限",
  "features.shareDialog.toast.publicLinkCreated": "公开链接已创建并复制",
  "features.shareDialog.toast.publicLinkCreateFailed": "无法创建公开链接",
  "features.shareDialog.toast.publicLinkRevoked": "公开链接已撤销",
  "features.shareDialog.toast.publicLinkRevokeFailed": "无法撤销公开链接",

  // 上传流程（`features/uploads/store.ts`）。会话异常、上传暂停等诊断信息保留在源码中。
  "features.uploads.batchName": {
    one: "{count} 个文件",
    other: "{count} 个文件",
  },
  "features.uploads.originalFileUnavailable": "原始文件已不可用，请重新开始上传。",

  // 登录校验（`auth/login-schema.ts`）。
  "features.auth.phoneFormat": "请使用 E.164 格式，并包含国家代码。",
  "features.auth.codeRequired": "请输入 Telegram 发送的验证码。",
  "features.auth.passwordRequired": "请输入你的 Telegram 两步验证密码。",

  // 表单脚手架（`forms/app-form.tsx`）。
  "features.form.errorSeparator": "；",
};
