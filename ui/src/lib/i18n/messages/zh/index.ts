/**
 * The Chinese catalog, merged the same way as the English one.
 *
 * The whole object is checked against `AreaMessages<typeof en>`: a key that is
 * missing, unknown, or has the wrong kind (a plural written as fixed text, or the
 * other way round) fails `bun run typecheck`.
 */
import type { AreaMessages } from "../../types";
import type { en } from "./../en";
import { common } from "./common";
import { components } from "./components";
import { errors } from "./errors";
import { features } from "./features";
import { routes } from "./routes";
import { settings } from "./settings";

export const zh: AreaMessages<typeof en> = {
  ...common,
  ...components,
  ...errors,
  ...features,
  ...routes,
  ...settings,
};
