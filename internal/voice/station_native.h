#ifndef STATION_NATIVE_H
#define STATION_NATIVE_H
#include <stddef.h>
#include <stdint.h>

typedef struct station_spotter station_spotter;
station_spotter *station_spotter_open(const char *model, char *error,
                                      size_t size);
int station_spotter_feed(station_spotter *s, const int16_t *pcm, size_t bytes,
                         char *error, size_t size);
int station_spotter_close(station_spotter *s, char *error, size_t size);
#endif
