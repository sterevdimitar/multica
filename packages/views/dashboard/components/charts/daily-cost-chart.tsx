import { BarChart, Bar, XAxis, YAxis, CartesianGrid } from "recharts";
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from "@multica/ui/components/ui/chart";
import type { DailyCostPoint } from "../../utils";

// ONE series, not the input / output / cache-write stack the runtime page
// draws. The dashboard plots the STORED cost of each run — the pipeline
// prices a run by the engine that actually ran it — and that figure is a
// total; splitting it back into parts would be fabricating a breakdown from
// a price table the fork does not have. Axis, grid and tooltip styling are
// kept identical to the runtimes chart so the two read as one family.
export const dailyCostConfig = {
  total: { label: "Cost", color: "var(--chart-1)" },
} satisfies ChartConfig;

export function DailyCostChart({ data }: { data: DailyCostPoint[] }) {
  // No internal empty-state — the parent decides what to show in place of
  // the chart, same as the runtimes charts.
  return (
    <ChartContainer config={dailyCostConfig} className="aspect-[3/1] w-full">
      <BarChart data={data} margin={{ left: 0, right: 0, top: 4, bottom: 0 }}>
        <CartesianGrid vertical={false} />
        <XAxis
          dataKey="label"
          tickLine={false}
          axisLine={false}
          tickMargin={8}
          interval="preserveStartEnd"
        />
        <YAxis
          tickLine={false}
          axisLine={false}
          tickMargin={8}
          tickFormatter={(v: number) => `$${v}`}
          width={50}
        />
        <ChartTooltip
          content={
            <ChartTooltipContent
              formatter={(value, name) =>
                typeof value === "number" ? `$${value.toFixed(2)} ${name}` : `${value} ${name}`
              }
            />
          }
        />
        <Bar dataKey="total" fill="var(--color-total)" radius={[3, 3, 0, 0]} />
      </BarChart>
    </ChartContainer>
  );
}
