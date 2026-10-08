/**
 * The English catalog: every area merged into the flat key space the `t()`
 * function looks up. This file is the source of truth for the key set; the
 * Chinese catalog is typed against it, so both stay in step.
 */
import { common } from "./common";
import { components } from "./components";
import { errors } from "./errors";
import { features } from "./features";
import { routes } from "./routes";
import { settings } from "./settings";

/** English messages by key, merged from the area catalogs above; its shape defines `MessageKey`. */
export const en = {
  ...common,
  ...components,
  ...errors,
  ...features,
  ...routes,
  ...settings,
};

/** Every message key the interface may ask for. */
export type MessageKey = keyof typeof en;
