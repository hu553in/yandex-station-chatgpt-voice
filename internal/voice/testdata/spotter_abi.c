#define dlopen fixture_dlopen
#define dlsym fixture_dlsym
#define dlclose fixture_dlclose
#include "../station_native.c"
#include <assert.h>
#include <stdlib.h>
#include <string.h>

static int conf, failure;
static int freed_errors, freed_confs, closed_libraries;

static const char *fixture_error_message(const void *err) {
  assert(err == &failure);
  return "constructor failed";
}

static void fixture_error_free(void **err) {
  assert(*err == &failure);
  *err = NULL;
  freed_errors++;
}

static void *fixture_conf_read(const char *model, void **err) {
  assert(strcmp(model, "fixture") == 0 && *err == NULL);
  return &conf;
}

static uint32_t fixture_sample_rate(const void *model, void **err) {
  assert(model == &conf && *err == NULL);
  return 16000;
}

static void *fixture_create(const void *model, uint32_t rate, uint32_t option,
                            void **err) {
  assert(model == &conf && rate == 16000 && option == 0);
  assert(err != NULL && *err == NULL);
  *err = &failure;
  return NULL;
}

static void fixture_conf_free(void *model, void **err) {
  assert(model == &conf && *err == NULL);
  freed_confs++;
}

static void fixture_unused(void) { abort(); }

void *fixture_dlopen(const char *path, int flags) {
  (void)path;
  (void)flags;
  return &conf;
}

int fixture_dlclose(void *library) {
  assert(library == &conf);
  closed_libraries++;
  return 0;
}

void *fixture_dlsym(void *library, const char *symbol) {
  assert(library == &conf);
  if (strcmp(symbol, "spotter_error_get_message") == 0)
    return (void *)fixture_error_message;
  if (strcmp(symbol, "spotter_error_free") == 0)
    return (void *)fixture_error_free;
  if (strcmp(symbol, "spotter_conf_read") == 0)
    return (void *)fixture_conf_read;
  if (strcmp(symbol, "spotter_conf_native_sample_rate") == 0)
    return (void *)fixture_sample_rate;
  if (strcmp(symbol, "spotter_create") == 0)
    return (void *)fixture_create;
  if (strcmp(symbol, "spotter_conf_free") == 0)
    return (void *)fixture_conf_free;
  return (void *)fixture_unused;
}

int main(void) {
  char error[512] = {0};
  assert(station_spotter_open("fixture", error, sizeof(error)) == NULL);
  assert(strcmp(error, "native spotter: constructor failed") == 0);
  assert(freed_errors == 1 && freed_confs == 1 && closed_libraries == 3);
  return 0;
}
