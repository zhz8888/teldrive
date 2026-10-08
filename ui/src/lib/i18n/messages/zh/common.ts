/**
 * 通用中文词条：应用名、通用操作与各处复用的标签。
 *
 * 键与 `messages/en/common.ts` 一一对应。
 */
import type { AreaMessages } from "../../types";
import type { common as enCommon } from "../en/common";

/**
 * Wording reused everywhere: the app name, shared action labels, states,
 * accessibility labels and language names.
 */
export const common: AreaMessages<typeof enCommon> = {
  "common.app.name": "Teldrive",
  "common.app.tagline": "基于 Telegram 的云存储",

  "common.action.add": "添加",
  "common.action.apply": "应用",
  "common.action.back": "返回",
  "common.action.cancel": "取消",
  "common.action.clear": "清除",
  "common.action.close": "关闭",
  "common.action.confirm": "确认",
  "common.action.copy": "复制",
  "common.action.create": "创建",
  "common.action.delete": "删除",
  "common.action.dismiss": "忽略",
  "common.action.done": "完成",
  "common.action.download": "下载",
  "common.action.edit": "编辑",
  "common.action.move": "移动",
  "common.action.next": "下一步",
  "common.action.open": "打开",
  "common.action.refresh": "刷新",
  "common.action.remove": "移除",
  "common.action.rename": "重命名",
  "common.action.reset": "重置",
  "common.action.retry": "重试",
  "common.action.save": "保存",
  "common.action.search": "搜索",
  "common.action.selectAll": "全选",
  "common.action.share": "分享",
  "common.action.upload": "上传",
  "common.action.view": "查看",

  "common.label.actions": "操作",
  "common.label.created": "创建时间",
  "common.label.description": "说明",
  "common.label.details": "详情",
  "common.label.email": "邮箱",
  "common.label.modified": "修改时间",
  "common.label.name": "名称",
  "common.label.none": "无",
  "common.label.owner": "所有者",
  "common.label.password": "密码",
  "common.label.size": "大小",
  "common.label.status": "状态",
  "common.label.type": "类型",
  "common.label.unknown": "未知",
  "common.label.username": "用户名",
  "common.label.version": "版本",

  "common.state.empty": "暂无内容。",
  "common.state.error": "出错了。",
  "common.state.loading": "加载中…",

  "common.a11y.closeDialog": "关闭对话框",
  "common.a11y.loading": "加载中",
  "common.a11y.toggleMenu": "展开或收起导航菜单",

  "common.locale.en": "English",
  "common.locale.zh": "简体中文",
};
