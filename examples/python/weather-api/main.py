from cortex_axon.handler import cortex_scheduled
from cortex_axon.axon_client import AxonClient
from jinja2 import Template
import json

from weather_data import WeatherData, load_cities

# In this example, we use the open source weather API
# to get the weather data for a specific


cities = load_cities()


def tagify(s):
    # replace non-alphanumeric characters with hyphens
    return "".join(c if c.isalnum() else "-" for c in s).lower()


# At startup, we create the city entities and their parent domains
# then do a run to collect data.
@cortex_scheduled(run_now=True)
def initialize_entities(context):

    # On startup, we pull the city list and create domains for each city, by continent,
    # Then we create a country domain, and finally cities

    # Ensure our custom city entity type
    response = context.cortex_api_call(
        method="POST",
        path="/api/v1/catalog/definitions",
        body="""
            {
            "description": "City entity type",
            "name": "City",
            "schema": {},
            "type": "city"
            }
        """,
    )

    if response.status_code >= 400 and response.status_code != 409:
        context.log(f"Failed to create city entity type: {response.text}")
        return

    # Create a root parent-domain for all entities
    ensure_domain(context, "Weather", "weather-data")

    seen_continents = {}
    for city in cities:

        continent = city.continent()
        if city.continent() not in seen_continents:

            seen_continents[continent] = "continent-" + tagify(continent)
            ensure_domain(
                context, continent, seen_continents[continent], parent="weather-data"
            )

        # add the country domain
        country_tag = "country-" + tagify(city.country)
        ensure_domain(
            context, city.country, country_tag, parent=seen_continents[continent]
        )

        # add the city entity
        ensure_city(context, city, country_tag)

    # Kick off a weather update
    update_weather(context)


# This function is called every hour to update the weather data
@cortex_scheduled(interval="1h")
def update_weather(context):
    # here, every hour we update the meteo data for each city
    # to reflect current conditions
    weather_data = WeatherData()
    city_weathers = weather_data.fetch(cities)

    # Custom data is like
    # {
    # "values": {
    #     "city-name": [
    #     {
    #         "key": "temperature",
    #         "value": "20°C"
    #     },
    #     {
    #         "key": "humidity",
    #         "value": "34%"
    #     },
    #     ...
    #     ],
    # }
    # }

    values = {}
    custom_data = {"values": values}

    for city_weather in city_weathers:
        city_tag = "city-" + tagify(city_weather.city.name)

        local_time_str = city_weather.datetime().strftime("%a %d %b %Y %I:%M %p")

        data = {
            "local_time_iso": city_weather.datetime().isoformat(),
            "local_time": local_time_str,
            "temperature": f"{round(city_weather.temperature, 1)}°C",
            "humidity": f"{city_weather.humidity}%",
            "is_day": True if city_weather.is_day == 1.0 else False,
            "cloud_cover": f"{city_weather.cloud_cover}%",
            "precipitation": f"{city_weather.precipitation}mm",
        }

        for k, v in data.items():
            values.setdefault(city_tag, []).append({"key": k, "value": v})

        body = json.dumps(custom_data)

        response = context.cortex_api_call(
            method="PUT",
            path="/api/v1/catalog/custom-data",
            body=body,
        )

        if response.status_code == 200:
            context.log(f"Updated weather for {city_weather.city.name}")
        else:
            context.log(
                f"Failed to update weather for {city_weather.city.name}: {response.text}"
            )


domain_yaml_template = """
openapi: 3.0.1
info:
  title: {{title}}
  x-cortex-tag: {{tag_name}}
  x-cortex-type: domain
  {% if parent %}
  x-cortex-parents:
    - tag: {{parent}}
  {% endif %}
  x-cortex-groups:
    - weather
"""

domain_template = Template(domain_yaml_template)


def ensure_domain(context, title, tag, parent=None):
    domain_yaml = domain_template.render(
        {"title": title, "tag_name": tag, "parent": parent}
    )
    context.log(f"Ensuring domain for {title} exists")
    response = context.cortex_api_call(
        method="PATCH",
        path="/api/v1/open-api",
        body=domain_yaml,
        content_type="application/openapi;charset=UTF-8",
    )
    if response.status_code == 201:
        context.log(f"Domain for {title} created")
    elif response.status_code >= 400:
        context.log(f"Error creating domain for {title}: {response.text}")


city_yaml_template = """
openapi: 3.0.1
info:
  title: {{name}}
  x-cortex-tag: {{tag_name}}
  x-cortex-type: city
  x-cortex-parents:
    - tag: {{country_tag}}
  x-cortex-groups:
    - tag: weather
"""

city_template = Template(city_yaml_template)


def ensure_city(context, city, country_tag):
    city_yaml = city_template.render(
        {
            "name": city.name,
            "tag_name": "city-" + tagify(city.name),
            "country_tag": country_tag,
        }
    )

    response = context.cortex_api_call(
        method="PATCH",
        path="/api/v1/open-api",
        body=city_yaml,
        content_type="application/openapi;charset=UTF-8",
    )
    if response.status_code == 201:
        context.log(f"City for {city} created")
    elif response.status_code >= 400:
        context.log(f"Error creating city for {city}: {response.text}")


def run():
    print("Starting weather-api")

    # Connect to the gRPC server
    client = AxonClient(
        scope=globals(),
    )
    client.run()


if __name__ == "__main__":
    run()
