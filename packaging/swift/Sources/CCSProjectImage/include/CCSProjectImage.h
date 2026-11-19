#ifndef CCSPROJECTIMAGE_H
#define CCSPROJECTIMAGE_H

const unsigned char *cs_project_image_bytes(void);
unsigned int cs_project_image_len(void);
const char *cs_project_image_sha256(void);
const char *cs_project_image_reference(void);

#endif
