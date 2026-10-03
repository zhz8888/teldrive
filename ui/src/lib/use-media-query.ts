import { useEffect, useState } from "react";

/**
 * Reports whether the viewport currently matches `query`, and keeps reporting as it
 * changes. The initial value is read during the first render so a reader mounts the
 * layout its viewport actually needs instead of swapping it afterwards, which would
 * mount both the wide and the narrow arrangement for a moment.
 */
export function useMediaQuery(query: string) {
  const [matches, setMatches] = useState(() =>
    typeof window === "undefined" ? false : window.matchMedia(query).matches,
  );

  useEffect(() => {
    const media = window.matchMedia(query);
    const update = () => setMatches(media.matches);
    update();
    media.addEventListener("change", update);
    return () => media.removeEventListener("change", update);
  }, [query]);

  return matches;
}
