#define _POSIX_C_SOURCE 200809L
#include "station_native.h"
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

struct station_spotter {
  void *iconv, *tflite, *library, *conf, *detector;
  const char *(*error_message)(const void *);
  void (*error_free)(void **);
  void *(*conf_read)(const char *, void **);
  uint32_t (*sample_rate)(const void *, void **);
  void *(*create)(const void *, uint32_t, uint32_t, void **);
  void (*submit)(void *, const void *, size_t, void **);
  int32_t (*next_event)(void *, void **);
  int32_t (*phrase_id)(void *, void **);
  const char *(*phrase)(void *, void **);
  void (*destroy)(void *, void **);
  void (*conf_free)(void *, void **);
};

static int vendor_error(station_spotter *s, void **error, char *out,
                        size_t size) {
  if (!*error)
    return 0;
  const char *message = s->error_message(*error);
  snprintf(out, size, "native spotter: %s", message ? message : "vendor error");
  s->error_free(error);
  *error = NULL;
  return -1;
}

int station_spotter_close(station_spotter *s, char *error, size_t size) {
  if (!s)
    return 0;
  int failed = 0;
  void *err = NULL;
  if (s->detector) {
    s->destroy(s->detector, &err);
    if (vendor_error(s, &err, error, size))
      failed = -1;
  }
  if (s->conf) {
    s->conf_free(s->conf, &err);
    if (vendor_error(s, &err, error, size))
      failed = -1;
  }
  if (s->library)
    dlclose(s->library);
  if (s->tflite)
    dlclose(s->tflite);
  if (s->iconv)
    dlclose(s->iconv);
  free(s);
  return failed;
}

#define LOAD(member, symbol)                                                   \
  do {                                                                         \
    *(void **)(&s->member) = dlsym(s->library, symbol);                        \
    if (!s->member) {                                                          \
      snprintf(error, size, "missing vendor symbol %s", symbol);               \
      goto fail;                                                               \
    }                                                                          \
  } while (0)

station_spotter *station_spotter_open(const char *model, char *error,
                                      size_t size) {
  station_spotter *s = calloc(1, sizeof(*s));
  if (!s) {
    snprintf(error, size, "cannot allocate native spotter");
    return NULL;
  }
  s->iconv =
      dlopen("/system/vendor/quasar/libs/libiconv.so", RTLD_NOW | RTLD_GLOBAL);
  if (!s->iconv)
    goto library_error;
  s->tflite =
      dlopen("/system/vendor/quasar/libs/libtflite.so", RTLD_NOW | RTLD_GLOBAL);
  if (!s->tflite)
    goto library_error;
  s->library = dlopen("/system/vendor/quasar/libs/libyandex_evil.so",
                      RTLD_NOW | RTLD_LOCAL);
  if (!s->library)
    goto library_error;
  LOAD(error_message, "spotter_error_get_message");
  LOAD(error_free, "spotter_error_free");
  LOAD(conf_read, "spotter_conf_read");
  LOAD(sample_rate, "spotter_conf_native_sample_rate");
  LOAD(create, "spotter_create");
  LOAD(submit, "spotter_submit_raw_data");
  LOAD(next_event, "spotter_get_next_event");
  LOAD(phrase_id, "spotter_get_current_phrase_id");
  LOAD(phrase, "spotter_get_current_phrase");
  LOAD(destroy, "spotter_destroy");
  LOAD(conf_free, "spotter_conf_free");
  void *err = NULL;
  s->conf = s->conf_read(model, &err);
  if (vendor_error(s, &err, error, size))
    goto fail;
  if (!s->conf) {
    snprintf(error, size, "vendor returned no configuration");
    goto fail;
  }
  uint32_t rate = s->sample_rate(s->conf, &err);
  if (vendor_error(s, &err, error, size))
    goto fail;
  if (rate != 16000) {
    snprintf(error, size, "unexpected native spotter sample rate");
    goto fail;
  }
  s->detector = s->create(s->conf, rate, 0, &err);
  if (vendor_error(s, &err, error, size))
    goto fail;
  if (!s->detector) {
    snprintf(error, size, "vendor returned no detector");
    goto fail;
  }
  return s;
library_error:
  snprintf(error, size, "cannot load Station vendor library: %s", dlerror());
fail:
  {
    char ignored[256];
    station_spotter_close(s, ignored, sizeof(ignored));
    return NULL;
  }
}

int station_spotter_feed(station_spotter *s, const int16_t *pcm, size_t bytes,
                         char *error, size_t size) {
  if (bytes != 640) {
    snprintf(error, size, "native spotter requires 640 PCM bytes");
    return -1;
  }
  void *err = NULL;
  s->submit(s->detector, pcm, bytes, &err);
  if (vendor_error(s, &err, error, size))
    return -1;
  int confirmed = 0;
  for (int count = 0; count < 64; count++) {
    int32_t event = s->next_event(s->detector, &err);
    if (vendor_error(s, &err, error, size))
      return -1;
    if (event == 0)
      return confirmed;
    if (event == 2 || event == 5) {
      int32_t phrase_id = s->phrase_id(s->detector, &err);
      if (vendor_error(s, &err, error, size))
        return -1;
      const char *phrase = s->phrase(s->detector, &err);
      if (vendor_error(s, &err, error, size))
        return -1;
      if (phrase_id == 1 && phrase && strcmp(phrase, "яндекс") == 0)
        confirmed = 1;
    }
  }
  snprintf(error, size, "native spotter event queue did not drain");
  return -1;
}
