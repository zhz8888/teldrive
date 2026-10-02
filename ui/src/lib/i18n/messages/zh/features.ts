/**
 * 功能模块的中文词条。
 *
 * 键与 `messages/en/features.ts` 一一对应。
 */
import type { AreaMessages } from "../../types";
import type { features as enFeatures } from "../en/features";

export const features: AreaMessages<typeof enFeatures> = {};
