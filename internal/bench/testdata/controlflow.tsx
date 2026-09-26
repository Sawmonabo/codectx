// One TSX component per group of the forms TSX adds over TypeScript: JSX
// elements with typed props, conditional rendering, a list rendered by a
// callback, and fragments, next to the TypeScript annotations and wrappers
// they carry. It is a benchmark input, not a proof of coverage: the
// lowering's golden tests are that.

interface Props {
  items: string[];
  selected?: string;
  onPick(item: string): void;
}

export function List({ items, selected, onPick }: Props) {
  if (items.length === 0) {
    return <p className="empty">Nothing here</p>;
  }
  return (
    <ul>
      {items.map((item) => (
        <li key={item} className={item === selected ? "on" : "off"} onClick={() => onPick(item)}>
          {item}
        </li>
      ))}
    </ul>
  );
}

export function Panel(props: { title?: string; children: unknown }) {
  const title = props.title ?? "Untitled";
  let body = props.children as string;
  for (const ch of title) {
    if (ch === "!") break;
    body += ch;
  }
  return (
    <>
      <List items={[title]} onPick={(s) => console.log(s!)} />
      {body.length > 0 && <section>{body}</section>}
    </>
  );
}
