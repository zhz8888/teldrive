/**
 * 设置界面的中文词条。
 *
 * 键与 `messages/en/settings.ts` 一一对应，类型由英文目录推导，缺键或多键都会在
 * 类型检查时报错。
 */
import type { AreaMessages } from "../../types";
import type { settings as enSettings } from "../en/settings";

export const settings: AreaMessages<typeof enSettings> = {
  "settings.appearance.title": "外观",
  "settings.appearance.description": "控制 Teldrive 在当前浏览器中的显示方式。",
  "settings.appearance.colorTheme.section": "配色主题",
  "settings.appearance.colorTheme.description": "选择会保存在本地并立即生效。",
  "settings.appearance.colorTheme.label": "主题",
  "settings.appearance.colorTheme.rowDescription": "选择浅色或深色的 Teldrive 视觉风格。",
  "settings.appearance.colorTheme.light": "浅色",
  "settings.appearance.colorTheme.dark": "深色",
  "settings.appearance.language.section": "语言",
  "settings.appearance.language.description": "选择会保存在本地并立即生效。",
  "settings.appearance.language.label": "界面语言",
  "settings.appearance.language.rowDescription": "选择菜单、标签和提示所使用的语言。日志输出始终保持英文。",
};
