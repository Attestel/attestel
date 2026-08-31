// Tiny classnames joiner — filters falsy, joins with spaces. No dependency needed.
export function cx(...parts) {
  return parts.filter(Boolean).join(" ");
}
