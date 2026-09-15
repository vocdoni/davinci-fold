interface Props {
  tally: number[]
}

export default function TallyChart({ tally }: Props) {
  const max = Math.max(...tally, 1)

  return (
    <div className="tally-chart">
      {tally.map((v, i) => (
        <div key={i} className="tally-row">
          <div className="tally-label">field {i}</div>
          <div className="tally-bar-wrap">
            <div
              className="tally-bar"
              style={{ width: `${(v / max) * 100}%` }}
            />
          </div>
          <div className="tally-val">{v.toLocaleString()}</div>
        </div>
      ))}
    </div>
  )
}
