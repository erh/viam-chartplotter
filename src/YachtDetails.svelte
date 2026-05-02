<script lang="ts">
  let {
    vicPowers,
    vicPowerNames,
    vicDoors,
  }: {
    vicPowers: Record<string, any>;
    vicPowerNames: string[];
    vicDoors: Record<string, boolean>;
  } = $props();

  function getDisplayValue(key: string, value: any): { label: string; value: string } {
    const lookup: Record<string, string> = {
      freq: "Frequency",
      kw: "Kilowatts",
      l1a: "L1A Current (A)",
      l1b: "L1B Current (A)",
      l1c: "L1C Current (A)",
      "vl-l": "Line-to-Line Voltage",
      "vl-n": "Line-to-Neutral Voltage",
    };

    const labels: Record<string, string> = {
      freq: "Hz",
      kw: "kW",
      l1a: "A",
      l1b: "A",
      l1c: "A",
      "vl-l": "V",
      "vl-n": "V",
    };

    const label = lookup[key] || key;
    const unit = labels[key] || "";
    const dividedValue = typeof value === "number" ? value / 10 : value;

    return {
      label,
      value: typeof dividedValue === "number" ? dividedValue.toFixed(2) : String(dividedValue),
      unit,
    };
  }
</script>

<div class="p-4 text-white">
  <h2 class="text-2xl font-bold mb-4 border-b border-gray-600 pb-2">Yacht Details</h2>
  <h3 class="text-xl font-semibold mb-3">Power Generation</h3>

  {#if Object.keys(vicPowers).length === 0}
    <div class="text-gray-400">Loading power data...</div>
  {:else}
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
      {#each vicPowerNames as powerName}
        {#if vicPowers[powerName]}
          <div class="bg-gray-900 rounded-lg p-4 border border-gray-700">
            <h3 class="text-lg font-semibold mb-3 capitalize text-blue-400">
              {powerName.replace("vic-power-", "").replace("-", " ")}
            </h3>

            <div class="grid grid-cols-2 gap-y-2 gap-x-4 text-sm">
              {#each Object.entries(vicPowers[powerName]).sort( (a, b) => a[0].localeCompare(b[0]) ) as [key, value]}
                <div class="flex justify-between items-center border-b border-gray-800 pb-1">
                  <span class="text-gray-400">{getDisplayValue(key, value).label}</span>
                  <span class="font-mono font-bold text-right">
                    {getDisplayValue(key, value).value}
                  </span>
                </div>
              {/each}
            </div>
          </div>
        {:else}
          <div class="bg-gray-900 rounded-lg p-4 border border-gray-700 opacity-50">
            <h3 class="text-lg font-semibold mb-3 capitalize">
              {powerName.replace("vic-power-", "").replace("-", " ")}
            </h3>
            <div class="text-gray-500">No data available</div>
          </div>
        {/if}
      {/each}
    </div>
  {/if}

  {#if vicDoors && Object.keys(vicDoors).length > 0}
    <h3 class="text-xl font-semibold mt-6 mb-3">Doors</h3>
    <div class="bg-gray-900 rounded-lg p-4 border border-gray-700">
      <div class="grid grid-cols-2 gap-y-2 gap-x-4 text-sm">
        {#each Object.entries(vicDoors).sort((a, b) => a[0].localeCompare(b[0])) as [name, isOpen]}
          <div class="flex justify-between items-center border-b border-gray-800 pb-1">
            <span class="text-gray-400 capitalize">{name.replaceAll("-", " ")}</span>
            <span class="font-mono font-bold {isOpen ? 'text-red-400' : 'text-green-400'}">
              {isOpen ? "Open" : "Closed"}
            </span>
          </div>
        {/each}
      </div>
    </div>
  {/if}
</div>
