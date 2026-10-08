/**
 * Client-side description of the Telegram login values: phone format, code and
 * password, with messages resolved at validation time.
 *
 * Nothing imports this module today — the login page holds the three values in
 * local state and lets the server reject what it cannot use — so these schemas
 * state the expected shape without being enforced anywhere yet.
 */
import { z } from "zod";
import { t } from "@/lib/i18n";

// The messages are functions so a validation error is worded in the locale that is
// active when the field is checked, not the one that was active at import time.
export const phoneNumberSchema = z
  .string()
  .regex(/^\+[1-9][0-9]{7,14}$/, { error: () => t("features.auth.phoneFormat") });

/** The code Telegram sends to the app; whitespace around a pasted code is ignored. */
export const telegramCodeSchema = z
  .string()
  .trim()
  .min(1, { error: () => t("features.auth.codeRequired") });

/** The account's two-step verification password, sent as the user typed it. */
export const telegramPasswordSchema = z
  .string()
  .min(1, { error: () => t("features.auth.passwordRequired") });

/**
 * Values the login form holds across its steps; only the fields of the current step
 * are checked.
 */
export type TelegramLoginValues = {
  /** The number as typed, in the `+<country><subscriber>` form `phoneNumberSchema` requires. */
  phoneNumber: string;
  /**
   * Login code Telegram sent for this flow; the code step is skipped when the start
   * call already asks for the password.
   */
  code: string;
  /** Two-step verification password, asked for only when the account has it enabled. */
  password: string;
};

/** Empty starting state for the login form, so every step begins on a blank field. */
export const defaultTelegramLoginValues: TelegramLoginValues = {
  phoneNumber: "",
  code: "",
  password: "",
};

/**
 * Picks the message to show for a field. TanStack Form reports an error either as
 * a string or as an object with a `message`, and a field can have several, so the
 * first usable one is returned; undefined means the field has nothing to report.
 */
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
