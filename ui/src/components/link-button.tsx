import { Button, type ButtonProps } from "@heroui/react";
import {
  Link,
  type AnyRouter,
  type LinkComponentProps,
  type RegisteredRouter,
} from "@tanstack/react-router";
import type { ReactNode, Ref } from "react";

/**
 * Button props plus the router link props of the target route. The link props
 * win where the two overlap, and `render` is dropped because the component
 * supplies its own render target.
 */
type LinkButtonProps<
  TRouter extends AnyRouter,
  TFrom extends string,
  TTo extends string | undefined,
  TMaskFrom extends string,
  TMaskTo extends string,
> = LinkComponentProps<"a", TRouter, TFrom, TTo, TMaskFrom, TMaskTo> &
  Omit<
    ButtonProps,
    keyof LinkComponentProps<"a", TRouter, TFrom, TTo, TMaskFrom, TMaskTo> | "render"
  > & {
    children?: ReactNode;
  };

/**
 * A router `Link` that looks and behaves like a button. Type parameters mirror
 * the router's, so `to`, `params` and `search` are checked against the route
 * tree exactly as they are on `Link`.
 */
export function LinkButton<
  TRouter extends AnyRouter = RegisteredRouter,
  const TFrom extends string = string,
  const TTo extends string | undefined = undefined,
  const TMaskFrom extends string = TFrom,
  const TMaskTo extends string = "",
>({ children, ...props }: LinkButtonProps<TRouter, TFrom, TTo, TMaskFrom, TMaskTo>) {
  const linkProps = props as LinkComponentProps<"a", TRouter, TFrom, TTo, TMaskFrom, TMaskTo>;

  return (
    <Button
      {...props}
      render={({ ref, ...buttonProps }) => {
        // @ts-expect-error HeroUI types render props for a button; this render target is an anchor.
        return <Link {...linkProps} {...buttonProps} ref={ref as Ref<HTMLAnchorElement>} />;
      }}
    >
      {children}
    </Button>
  );
}
