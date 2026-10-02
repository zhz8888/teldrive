import { z } from "zod";
import { t } from "@/lib/i18n";

// The messages are functions so a validation error is worded in the locale that is
// active when the field is checked, not the one that was active at import time.
export const phoneNumberSchema = z
  .string()
  .regex(/^\+[1-9][0-9]{7,14}$/, { error: () => t("features.auth.phoneFormat") });

export const telegramCodeSchema = z
  .string()
  .trim()
  .min(1, { error: () => t("features.auth.codeRequired") });

export const telegramPasswordSchema = z
  .string()
  .min(1, { error: () => t("features.auth.passwordRequired") });

export type TelegramLoginValues = {
  phoneNumber: string;
  code: string;
  password: string;
};

export const defaultTelegramLoginValues: TelegramLoginValues = {
  phoneNumber: "",
  code: "",
  password: "",
};

export function firstFieldError(errors: unknown[]) {
  for (const error of errors) {
    if (typeof error === "string") {
      return error;
    }
    if (error && typeof error === "object" && "message" in error) {
      const message = (error as { message?: unknown }).message;
      if (typeof message === "string") {
        return message;
      }
    }
  }
  return undefined;
}
