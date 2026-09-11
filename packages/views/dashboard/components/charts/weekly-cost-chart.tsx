import { BarChart, Bar, XAxis, YAxis, CartesianGrid, Cell } from "recharts";
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from "@multica/ui/components/ui/chart";
import type { WeeklyCostPoint } from "../../utils";
import { useT } from "../../../i18n";

// The weekly cut of DailyCostChart: same single stored-cost series, same
// styling, so "Weekly" reads as a coarser view of the same chart. Partial
// weeks render at half opacity, exactly as the runtimes weekly charts do.
export const weeklyCostConfig = {
  total: { label: "Cost", color: "var(--chart-1)" },
} satisfies ChartConfig;

export function WeeklyCostChart({ data }: { data: WeeklyCostPoint[] }) {
  const { t } = useT("runtimes");
  return (
    <ChartContainer config={weeklyCostConfig} className="aspect-[3/1] w-full">
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
              labelKey="rangeLabel"
              labelFormatter={(_label, payload) => {
                const row = payload[0]?.payload as WeeklyCostPoint | undefined;
                if (!row) return "";
                return row.partial
                  ? t(($) => $.usage.weekly_partial_label, {
                      range: row.rangeLabel,
                      covered: row.daysCovered,
                    })
                  : row.rangeLabel;
              }}
              formatter={(value, name) =>
                typeof value === "number" ? `$${value.toFixed(2)} ${name}` : `${value} ${name}`
              }
            />
          }
        />
        <Bar dataKey="total" fill="var(--color-total)" radius={[3, 3, 0, 0]}>
          {data.map((d) => (
            <Cell key={d.weekStart} fillOpacity={d.partial ? 0.5 : 1} />
          ))}
        </Bar>
      </BarChart>
    </ChartContainer>
  );
}
