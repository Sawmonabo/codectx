import React from "react";

// héllo → 日本
export const greeting = "héllo → 日本";

export interface Props { name: string }

/** A component with a nested helper. */
export function View(props: Props): React.ReactElement {
  const label = (): string => greet(props.name);
  return <div title={greeting}>{label()}</div>;
}

function greet(name: string): string {
  return greeting + name;
}
