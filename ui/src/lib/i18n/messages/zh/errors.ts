/**
 * 请求失败提示的中文词条。
 *
 * 键与 `messages/en/errors.ts` 一一对应。
 */
import type { AreaMessages } from "../../types";
import type { errors as enErrors } from "../en/errors";

export const errors: AreaMessages<typeof enErrors> = {};
