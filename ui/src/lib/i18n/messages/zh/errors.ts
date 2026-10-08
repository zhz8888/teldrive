/**
 * 请求失败提示的中文词条。
 *
 * 键与 `messages/en/errors.ts` 一一对应。
 */
import type { AreaMessages } from "../../types";
import type { errors as enErrors } from "../en/errors";

/**
 * Wording for the failures the interface phrases itself; a server-authored message
 * takes precedence where there is one.
 */
export const errors: AreaMessages<typeof enErrors> = {
  // `api/errors.ts` 中 `userMessage()` 的友好提示分支。若服务端自带文案，
  // 仍优先使用服务端文案，不使用下列词条。
  "errors.networkUnreachable": "无法连接到 Teldrive 服务器，请检查网络后重试。",
  "errors.badRequest": "请求内容无效。",
  "errors.sessionExpired": "登录状态已过期，请重新登录以继续。",
  "errors.forbidden": "你没有执行此操作的权限。",
  "errors.notFound": "请求的内容已不存在。",
  "errors.conflict": "该更改与现有内容冲突。",
  "errors.gone": "该上传或分享已过期。",
  "errors.preconditionFailed": "该项目已在其他设备上被修改，请刷新后重试。",
  "errors.payloadTooLarge": "所选文件超出服务器允许的大小。",
  "errors.rangeNotSatisfiable": "请求的文件范围不可用。",
  "errors.unprocessable": "部分内容需要修正。",
  "errors.tooManyRequests": "Teldrive 收到的请求过多，请稍后重试。",
  "errors.serverError": "Teldrive 遇到服务器错误，你的数据未发生更改。",
};
