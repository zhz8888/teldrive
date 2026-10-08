import { useEffect, useState } from "react";

/**
 * Trails `value` by `delay` milliseconds. Text fields that drive a request use it
 * so a pause in typing issues one call instead of one per keystroke, while the
 * input itself stays controlled by the undebounced value.
 */
export function useDebouncedValue<T>(value: T, delay: number): T {
  const [debouncedValue, setDebouncedValue] = useState(value);

  useEffect(() => {
    const timer = window.setTimeout(() => setDebouncedValue(() => value), delay);
    return () => window.clearTimeout(timer);
  }, [value, delay]);

  return debouncedValue;
}
